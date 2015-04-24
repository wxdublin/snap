package control

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/intelsdilabs/gomit"

	"github.com/intelsdilabs/pulse/control/plugin"
	"github.com/intelsdilabs/pulse/control/plugin/client"
	"github.com/intelsdilabs/pulse/control/routing"
	"github.com/intelsdilabs/pulse/core"
	"github.com/intelsdilabs/pulse/core/cdata"
	"github.com/intelsdilabs/pulse/core/control_event"
	"github.com/intelsdilabs/pulse/core/ctypes"
	"github.com/intelsdilabs/pulse/pkg/logger"
)

// control private key (RSA private key)
// control public key (RSA public key)
// Plugin token = token generated by plugin and passed to control
// Session token = plugin seed encrypted by control private key, verified by plugin using control public key
//

type executablePlugins []plugin.ExecutablePlugin

type pluginControl struct {
	// TODO, going to need coordination on changing of these
	RunningPlugins executablePlugins
	Started        bool

	controlPrivKey *rsa.PrivateKey
	controlPubKey  *rsa.PublicKey
	eventManager   *gomit.EventController

	pluginManager managesPlugins
	metricCatalog catalogsMetrics
	pluginRunner  runsPlugins

	strategy RoutingStrategy
}

type runsPlugins interface {
	Start() error
	Stop() []error
	AvailablePlugins() *availablePlugins
	AddDelegates(delegates ...gomit.Delegator)
	SetEmitter(emitter gomit.Emitter)
	SetMetricCatalog(c catalogsMetrics)
	SetPluginManager(m managesPlugins)
	Monitor() *monitor
}

type managesPlugins interface {
	LoadPlugin(string, gomit.Emitter) (*loadedPlugin, error)
	UnloadPlugin(CatalogedPlugin) error
	LoadedPlugins() *loadedPlugins
	SetMetricCatalog(catalogsMetrics)
	GenerateArgs(pluginPath string) plugin.Arg
}

type catalogsMetrics interface {
	Get([]string, int) (*metricType, error)
	Add(*metricType)
	AddLoadedMetricType(*loadedPlugin, core.MetricType)
	Fetch([]string) ([]*metricType, error)
	Item() (string, []*metricType)
	Next() bool
	Subscribe([]string, int) error
	Unsubscribe([]string, int) error
	GetPlugin([]string, int) (*loadedPlugin, error)
}

// New returns a new pluginControl instance
func New() *pluginControl {

	c := &pluginControl{}
	// Initialize components
	//
	// Event Manager
	c.eventManager = gomit.NewEventController()
	logger.Debug("control.init", "event controller created")

	// Metric Catalog
	c.metricCatalog = newMetricCatalog()
	logger.Debug("control.init", "metric catalog created")

	// Plugin Manager
	c.pluginManager = newPluginManager()
	logger.Debug("control.init", "plugin manager created")
	//    Plugin Manager needs a reference to the metric catalog
	c.pluginManager.SetMetricCatalog(c.metricCatalog)

	// Plugin Runner
	c.pluginRunner = newRunner()
	logger.Debug("control.init", "runner created")
	c.pluginRunner.AddDelegates(c.eventManager)
	c.pluginRunner.SetEmitter(c.eventManager) // emitter is passed to created availablePlugins
	c.pluginRunner.SetMetricCatalog(c.metricCatalog)
	c.pluginRunner.SetPluginManager(c.pluginManager)

	// Strategy
	c.strategy = &routing.RoundRobinStrategy{}

	// Wire event manager

	// Start stuff
	err := c.pluginRunner.Start()
	if err != nil {
		panic(err)
	}

	return c
}

// Begin handling load, unload, and inventory
func (p *pluginControl) Start() error {
	// Start pluginManager when pluginControl starts
	p.Started = true
	logger.Debug("control.start", "started")
	return nil
}

func (p *pluginControl) Stop() {
	p.Started = false
	logger.Debug("control.stop", "stopped")
}

// Load is the public method to load a plugin into
// the LoadedPlugins array and issue an event when
// successful.
func (p *pluginControl) Load(path string) error {
	// logger.Debug("control.load", fmt.Sprintf("load called on path: %s", path))
	if !p.Started {
		return errors.New("Must start Controller before calling Load()")
	}

	if _, err := p.pluginManager.LoadPlugin(path, p.eventManager); err != nil {
		return err
	}

	// defer sending event
	event := new(control_event.LoadPluginEvent)
	defer p.eventManager.Emit(event)
	return nil
}

func (p *pluginControl) Unload(pl CatalogedPlugin) error {
	err := p.pluginManager.UnloadPlugin(pl)
	if err != nil {
		return err
	}

	event := new(control_event.UnloadPluginEvent)
	defer p.eventManager.Emit(event)
	return nil
}

func (p *pluginControl) SwapPlugins(inPath string, out CatalogedPlugin) error {

	lp, err := p.pluginManager.LoadPlugin(inPath, p.eventManager)
	if err != nil {
		return err
	}

	err = p.pluginManager.UnloadPlugin(out)
	if err != nil {
		err2 := p.pluginManager.UnloadPlugin(lp)
		if err2 != nil {
			return errors.New("failed to rollback after error" + err2.Error() + " -- " + err.Error())
		}
		return err
	}

	event := new(control_event.SwapPluginsEvent)
	defer p.eventManager.Emit(event)

	return nil
}

// SubscribeMetricType validates the given config data, and if valid
// returns a MetricType with a config.  On error a collection of errors is returned
// either from config data processing, or the inability to find the metric.
func (p *pluginControl) SubscribeMetricType(mt core.MetricType, cd *cdata.ConfigDataNode) (core.MetricType, []error) {
	logger.Info("control.subscribe", fmt.Sprintf("subscription called with: %s", mt.Namespace()))
	var subErrs []error

	m, err := p.metricCatalog.Get(mt.Namespace(), mt.Version())
	if err != nil {
		subErrs = append(subErrs, err)
		return nil, subErrs
	}

	// No metric found return error.
	if m == nil {
		subErrs = append(subErrs, errors.New(fmt.Sprintf("no metric found cannot subscribe: (%s) version(%d)", mt.Namespace(), mt.Version())))
		return nil, subErrs
	}

	ncdTable, errs := m.policy.Process(cd.Table())
	if errs != nil && errs.HasErrors() {
		return nil, errs.Errors()
	}
	m.config = cdata.FromTable(*ncdTable)

	m.Subscribe()
	e := &control_event.MetricSubscriptionEvent{
		MetricNamespace: m.Namespace(),
		Version:         m.Version(),
	}
	defer p.eventManager.Emit(e)

	return m, nil
}

// SubscribePublisher
func (p *pluginControl) SubscribePublisher(name string, ver int, config map[string]ctypes.ConfigValue) []error {
	logger.Info("control.subscribe", fmt.Sprintf("publisher subscription called for %v:%v", name, ver))
	var subErrs []error

	p.pluginManager.LoadedPlugins().Lock()
	defer p.pluginManager.LoadedPlugins().Unlock()
	var lp *loadedPlugin
	for p.pluginManager.LoadedPlugins().Next() {
		_, l := p.pluginManager.LoadedPlugins().Item()
		if l.Name() == name && l.Version() == ver {
			lp = l
		}
	}

	if lp == nil {
		subErrs = append(subErrs, errors.New(fmt.Sprintf("No loaded plugin found for publisher name: %v version: %v", name, ver)))
		return subErrs
	}

	ncd := lp.ConfigPolicyTree.Get([]string{""})
	_, errs := ncd.Process(config)
	if errs != nil && errs.HasErrors() {
		return errs.Errors()
	}

	//TODO store subscription counts for publishers

	e := &control_event.PublisherSubscriptionEvent{
		PluginName:    name,
		PluginVersion: ver,
	}
	defer p.eventManager.Emit(e)

	return nil
}

// UnsubscribeMetricType unsubscribes a MetricType
// If subscriptions fall below zero we will panic.
func (p *pluginControl) UnsubscribeMetricType(mt core.MetricType) {
	logger.Info("control.subscribe", fmt.Sprintf("unsubscription called with: %s", mt.Namespace()))
	err := p.metricCatalog.Unsubscribe(mt.Namespace(), mt.Version())
	if err != nil {
		// panic because if a metric falls below 0, something bad has happened
		panic(err.Error())
	}
	e := &control_event.MetricUnsubscriptionEvent{
		MetricNamespace: mt.Namespace(),
	}
	p.eventManager.Emit(e)
}

// SetMonitorOptions exposes monitors options
func (p *pluginControl) SetMonitorOptions(options ...monitorOption) {
	p.pluginRunner.Monitor().Option(options...)
}

// the public interface for a plugin
// this should be the contract for
// how mgmt modules know a plugin
type CatalogedPlugin interface {
	Name() string
	Version() int
	TypeName() string
	Status() string
	LoadedTimestamp() int64
}

// the collection of cataloged plugins used
// by mgmt modules
type PluginCatalog []CatalogedPlugin

// returns a copy of the plugin catalog
func (p *pluginControl) PluginCatalog() PluginCatalog {
	table := p.pluginManager.LoadedPlugins().Table()
	pc := make([]CatalogedPlugin, len(table))
	for i, lp := range table {
		pc[i] = lp
	}
	return pc
}

func (p *pluginControl) MetricCatalog() ([]core.MetricType, error) {
	var c []core.MetricType
	mts, err := p.metricCatalog.Fetch([]string{})
	if err != nil {
		return nil, err
	}
	for _, mt := range mts {
		c = append(c, mt)
	}

	return c, nil
}

func (p *pluginControl) MetricExists(mns []string, ver int) bool {
	_, err := p.metricCatalog.Get(mns, ver)
	if err == nil {
		return true
	}
	return false
}

// CollectMetrics is a blocking call to collector plugins returning a collection
// of metrics and errors.  If an error is encountered no metrics will be
// returned.
func (p *pluginControl) CollectMetrics(
	metricTypes []core.MetricType,
	deadline time.Time,
) (metrics []core.Metric, errs []error) {

	pluginToMetricMap, err := groupMetricTypesByPlugin(p.metricCatalog, metricTypes)
	if err != nil {
		errs = append(errs, err)
		return
	}

	cMetrics := make(chan []core.Metric)
	cError := make(chan error)
	var wg sync.WaitGroup

	// For each available plugin call available plugin using RPC client and wait for response (goroutines)
	for pluginKey, pmt := range pluginToMetricMap {

		// resolve a pool (from catalog)
		pool, err := getPool(pluginKey, p.pluginRunner.AvailablePlugins())
		if err != nil {
			errs = append(errs, err)
			continue
		}

		// resolve a available plugin from pool
		ap, err := getAvailablePlugin(pool, p.strategy)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		// cast client to PluginCollectorClient
		cli, ok := ap.Client.(client.PluginCollectorClient)
		if !ok {
			err := errors.New("unable to cast client to PluginCollectorClient")
			errs = append(errs, err)
			continue
		}

		wg.Add(1)

		// get a metrics
		go func(mt []core.MetricType) {
			metrics, err = cli.CollectMetrics(mt)
			if err != nil {
				cError <- err
			} else {
				cMetrics <- metrics
			}
		}(pmt.metricTypes)

		// update statics about plugin
		ap.hitCount++
		ap.lastHitTime = time.Now()
	}

	go func() {
		for m := range cMetrics {
			metrics = append(metrics, m...)
			wg.Done()
		}
	}()

	go func() {
		for e := range cError {
			errs = append(errs, e)
			wg.Done()
		}
	}()

	wg.Wait()
	close(cMetrics)
	close(cError)

	if len(errs) > 0 {
		return nil, errs
	}
	return
}

// PublishMetrics
func (p *pluginControl) PublishMetrics(contentType string, content []byte, pluginName string, pluginVersion int, config map[string]ctypes.ConfigValue) []error {
	key := strings.Join([]string{pluginName, strconv.Itoa(pluginVersion)}, ":")

	pool := p.pluginRunner.AvailablePlugins().Publishers.GetPluginPool(key)
	if pool == nil {
		return []error{errors.New(fmt.Sprintf("No available plugin found for %v:%v", pluginName, pluginVersion))}
	}

	// resolve a available plugin from pool
	ap, err := getAvailablePlugin(pool, p.strategy)
	if err != nil {
		return []error{err}
	}

	cli, ok := ap.Client.(client.PluginPublisherClient)
	if !ok {
		return []error{errors.New("unable to cast client to PluginPublisherClient")}
	}

	err = cli.Publish(contentType, content, config)
	if err != nil {
		return []error{err}
	}
	return nil
}

// ------------------- helper struct and function for grouping metrics types ------

// just a tuple of loadedPlugin and metricType slice
type pluginMetricTypes struct {
	plugin      *loadedPlugin
	metricTypes []core.MetricType
}

func (p *pluginMetricTypes) Count() int {
	return len(p.metricTypes)
}

// groupMetricTypesByPlugin groups metricTypes by a plugin.Key() and returns appropriate structure
func groupMetricTypesByPlugin(cat catalogsMetrics, metricTypes []core.MetricType) (map[string]pluginMetricTypes, error) {
	pmts := make(map[string]pluginMetricTypes)
	// For each plugin type select a matching available plugin to call
	for _, mt := range metricTypes {

		// This is set to choose the newest and not pin version. TODO, be sure version is set to -1 if not provided by user on Task creation.
		lp, err := cat.GetPlugin(mt.Namespace(), -1)
		if err != nil {
			return nil, err
		}
		// if loaded plugin is nil, we have failed.  return error
		if lp == nil {
			return nil, errors.New(fmt.Sprintf("Metric missing: %s", strings.Join(mt.Namespace(), "/")))
		}

		// fmt.Printf("Found plugin (%s v%d) for metric (%s)\n", lp.Name(), lp.Version(), strings.Join(m.Namespace(), "/"))

		key := lp.Key()

		//
		pmt, _ := pmts[key]
		pmt.plugin = lp
		pmt.metricTypes = append(pmt.metricTypes, mt)
		pmts[key] = pmt

	}
	return pmts, nil
}

// getPool finds a pool for a given pluginKey and checks is not empty
func getPool(pluginKey string, availablePlugins *availablePlugins) (*availablePluginPool, error) {

	pool := availablePlugins.Collectors.GetPluginPool(pluginKey)

	if pool == nil {
		// return error because this plugin has no pool
		return nil, errors.New(fmt.Sprintf("no available plugins for plugin type (%s)", pluginKey))
	}

	// TODO: Lock this apPool so we are the only one operating on it.
	if pool.Count() == 0 {
		// return error indicating we have no available plugins to call for Collect
		return nil, errors.New(fmt.Sprintf("there is no availablePlugins in pool (%s)", pluginKey))
	}
	return pool, nil
}

// getAvailablePlugin finds a "best" availablePlugin to be asked for metrics
func getAvailablePlugin(pool *availablePluginPool, strategy RoutingStrategy) (*availablePlugin, error) {

	// Use a router strategy to select an available plugin from the pool
	ap, err := pool.SelectUsingStrategy(strategy)
	if err != nil {
		return nil, err
	}

	if ap == nil {
		return nil, errors.New(fmt.Sprintf("no available plugin selected in pool %v", pool))
	}

	return ap, nil
}
