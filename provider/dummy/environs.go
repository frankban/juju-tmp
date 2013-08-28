// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

// The dummy provider implements an environment provider for testing
// purposes, registered with environs under the name "dummy".
//
// The configuration YAML for the testing environment
// must specify a "state-server" property with a boolean
// value. If this is true, a state server will be started
// the first time StateInfo is called on a newly reset environment.
//
// The configuration data also accepts a "broken" property
// of type boolean. If this is non-empty, any operation
// after the environment has been opened will return
// the error "broken environment", and will also log that.
//
// The DNS name of instances is the same as the Id,
// with ".dns" appended.
//
// To avoid enumerating all possible series and architectures,
// any series or architecture with the prefix "unknown" is
// treated as bad when starting a new instance.
package dummy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/cloudinit"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/environs/imagemetadata"
	envtesting "launchpad.net/juju-core/environs/testing"
	"launchpad.net/juju-core/environs/tools"
	"launchpad.net/juju-core/instance"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/names"
	"launchpad.net/juju-core/schema"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
	"launchpad.net/juju-core/state/apiserver"
	"launchpad.net/juju-core/testing"
	coretools "launchpad.net/juju-core/tools"
	"launchpad.net/juju-core/utils"
)

// stateInfo returns a *state.Info which allows clients to connect to the
// shared dummy state, if it exists.
func stateInfo() *state.Info {
	if testing.MgoAddr == "" {
		panic("dummy environ state tests must be run with MgoTestPackage")
	}
	return &state.Info{
		Addrs:  []string{testing.MgoAddr},
		CACert: []byte(testing.CACert),
	}
}

// Operation represents an action on the dummy provider.
type Operation interface{}

type GenericOperation struct {
	Env string
}

type OpBootstrap struct {
	Env         string
	Constraints constraints.Value
}

type OpDestroy GenericOperation

type OpStartInstance struct {
	Env          string
	MachineId    string
	MachineNonce string
	Instance     instance.Instance
	Constraints  constraints.Value
	Info         *state.Info
	APIInfo      *api.Info
	Secret       string
}

type OpStopInstances struct {
	Env       string
	Instances []instance.Instance
}

type OpOpenPorts struct {
	Env        string
	MachineId  string
	InstanceId instance.Id
	Ports      []instance.Port
}

type OpClosePorts struct {
	Env        string
	MachineId  string
	InstanceId instance.Id
	Ports      []instance.Port
}

type OpPutFile struct {
	Env      string
	FileName string
}

// environProvider represents the dummy provider.  There is only ever one
// instance of this type (providerInstance)
type environProvider struct {
	mu  sync.Mutex
	ops chan<- Operation
	// We have one state for each environment name
	state map[string]*environState
}

var providerInstance environProvider

// environState represents the state of an environment.
// It can be shared between several environ values,
// so that a given environment can be opened several times.
type environState struct {
	name          string
	ops           chan<- Operation
	mu            sync.Mutex
	maxId         int // maximum instance id allocated so far.
	insts         map[instance.Id]*dummyInstance
	globalPorts   map[instance.Port]bool
	bootstrapped  bool
	storageDelay  time.Duration
	storage       *storage
	publicStorage *storage
	httpListener  net.Listener
	apiServer     *apiserver.Server
	apiState      *state.State
}

// environ represents a client's connection to a given environment's
// state.
type environ struct {
	name         string
	ecfgMutex    sync.Mutex
	ecfgUnlocked *environConfig
}

var _ imagemetadata.SupportsCustomURLs = (*environ)(nil)
var _ tools.SupportsCustomURLs = (*environ)(nil)
var _ environs.Environ = (*environ)(nil)

// storage holds the storage for an environState.
// There are two instances for each environState
// instance, one for public files and one for private.
type storage struct {
	path     string // path prefix in http space.
	state    *environState
	files    map[string][]byte
	poisoned map[string]error
}

// discardOperations discards all Operations written to it.
var discardOperations chan<- Operation

func init() {
	environs.RegisterProvider("dummy", &providerInstance)

	// Prime the first ops channel, so that naive clients can use
	// the testing environment by simply importing it.
	c := make(chan Operation)
	go func() {
		for _ = range c {
		}
	}()
	discardOperations = c
	Reset()

	// parse errors are ignored
	providerDelay, _ = time.ParseDuration(os.Getenv("JUJU_DUMMY_DELAY"))
}

// Reset resets the entire dummy environment and forgets any registered
// operation listener.  All opened environments after Reset will share
// the same underlying state.
func Reset() {
	log.Infof("environs/dummy: reset environment")
	p := &providerInstance
	p.mu.Lock()
	defer p.mu.Unlock()
	providerInstance.ops = discardOperations
	for _, s := range p.state {
		s.httpListener.Close()
		s.destroy()
	}
	providerInstance.state = make(map[string]*environState)
	if testing.MgoAddr != "" {
		testing.MgoReset()
	}
}

func (state *environState) destroy() {
	state.storage.files = make(map[string][]byte)
	if !state.bootstrapped {
		return
	}
	if state.apiServer != nil {
		if err := state.apiServer.Stop(); err != nil {
			panic(err)
		}
		state.apiServer = nil
		if err := state.apiState.Close(); err != nil {
			panic(err)
		}
		state.apiState = nil
	}
	if testing.MgoAddr != "" {
		testing.MgoReset()
	}
	state.bootstrapped = false
}

// GetStateInAPIServer returns the state connection used by the API server
// This is so code in the test suite can trigger Syncs, etc that the API server
// will see, which will then trigger API watchers, etc.
func (e *environ) GetStateInAPIServer() *state.State {
	return e.state().apiState
}

// newState creates the state for a new environment with the
// given name and starts an http server listening for
// storage requests.
func newState(name string, ops chan<- Operation) *environState {
	s := &environState{
		name:        name,
		ops:         ops,
		insts:       make(map[instance.Id]*dummyInstance),
		globalPorts: make(map[instance.Port]bool),
	}
	s.storage = newStorage(s, "/"+name+"/private")
	s.publicStorage = newStorage(s, "/"+name+"/public")
	s.listen()
	// TODO(fwereade): get rid of these.
	envtesting.MustUploadFakeTools(s.publicStorage)
	return s
}

// listen starts a network listener listening for http
// requests to retrieve files in the state's storage.
func (s *environState) listen() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Errorf("cannot start listener: %v", err))
	}
	s.httpListener = l
	mux := http.NewServeMux()
	mux.Handle(s.storage.path+"/", http.StripPrefix(s.storage.path+"/", s.storage))
	mux.Handle(s.publicStorage.path+"/", http.StripPrefix(s.publicStorage.path+"/", s.publicStorage))
	go http.Serve(l, mux)
}

// Listen closes the previously registered listener (if any).
// Subsequent operations on any dummy environment can be received on c
// (if not nil).
func Listen(c chan<- Operation) {
	p := &providerInstance
	p.mu.Lock()
	defer p.mu.Unlock()
	if c == nil {
		c = discardOperations
	}
	if p.ops != discardOperations {
		close(p.ops)
	}
	p.ops = c
	for _, st := range p.state {
		st.mu.Lock()
		st.ops = c
		st.mu.Unlock()
	}
}

// SetStorageDelay causes any storage download operation in any current
// environment to be delayed for the given duration.
func SetStorageDelay(d time.Duration) {
	p := &providerInstance
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.state {
		st.mu.Lock()
		st.storageDelay = d
		st.mu.Unlock()
	}
}

var configFields = schema.Fields{
	"state-server": schema.Bool(),
	"broken":       schema.String(),
	"secret":       schema.String(),
}
var configDefaults = schema.Defaults{
	"broken": "",
	"secret": "pork",
}

type environConfig struct {
	*config.Config
	attrs map[string]interface{}
}

func (c *environConfig) stateServer() bool {
	return c.attrs["state-server"].(bool)
}

func (c *environConfig) broken() string {
	return c.attrs["broken"].(string)
}

func (c *environConfig) secret() string {
	return c.attrs["secret"].(string)
}

func (p *environProvider) newConfig(cfg *config.Config) (*environConfig, error) {
	valid, err := p.Validate(cfg, nil)
	if err != nil {
		return nil, err
	}
	return &environConfig{valid, valid.UnknownAttrs()}, nil
}

func (p *environProvider) Validate(cfg, old *config.Config) (valid *config.Config, err error) {
	// Check for valid changes for the base config values.
	if err := config.Validate(cfg, old); err != nil {
		return nil, err
	}
	validated, err := cfg.ValidateUnknownAttrs(configFields, configDefaults)
	if err != nil {
		return nil, err
	}
	// Apply the coerced unknown values back into the config.
	return cfg.Apply(validated)
}

func (e *environ) state() *environState {
	p := &providerInstance
	p.mu.Lock()
	defer p.mu.Unlock()
	if state := p.state[e.name]; state != nil {
		return state
	}
	panic(fmt.Errorf("environment %q is not prepared", e.name))
}

func (p *environProvider) Open(cfg *config.Config) (environs.Environ, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ecfg, err := p.newConfig(cfg)
	if err != nil {
		return nil, err
	}
	env := &environ{
		name:         ecfg.Name(),
		ecfgUnlocked: ecfg,
	}
	if err := env.checkBroken("Open"); err != nil {
		return nil, err
	}
	return env, nil
}

func (p *environProvider) Prepare(cfg *config.Config) (environs.Environ, error) {
	ecfg, err := p.newConfig(cfg)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	name := cfg.Name()
	state := p.state[name]
	if state == nil {
		if ecfg.stateServer() && len(p.state) != 0 {
			var old string
			for oldName := range p.state {
				old = oldName
				break
			}
			panic(fmt.Errorf("cannot share a state between two dummy environs; old %q; new %q", old, name))
		}
		state = newState(name, p.ops)
		p.state[name] = state
	}
	// TODO(rog) add an attribute to the configuration which is required for Open?
	p.mu.Unlock()
	return p.Open(cfg)
}

func (*environProvider) SecretAttrs(cfg *config.Config) (map[string]interface{}, error) {
	m := make(map[string]interface{})
	ecfg, err := providerInstance.newConfig(cfg)
	if err != nil {
		return nil, err
	}
	m["secret"] = ecfg.secret()
	return m, nil

}

func (*environProvider) PublicAddress() (string, error) {
	return "public.dummy.address.example.com", nil
}

func (*environProvider) PrivateAddress() (string, error) {
	return "private.dummy.address.example.com", nil
}

func (*environProvider) BoilerplateConfig() string {
	return `
## Fake configuration for dummy provider.
dummy:
  type: dummy
  admin-secret: {{rand}}

`[1:]
}

var errBroken = errors.New("broken environment")

func (e *environ) ecfg() *environConfig {
	e.ecfgMutex.Lock()
	ecfg := e.ecfgUnlocked
	e.ecfgMutex.Unlock()
	return ecfg
}

func (e *environ) checkBroken(method string) error {
	for _, m := range strings.Fields(e.ecfg().broken()) {
		if m == method {
			return fmt.Errorf("dummy.%s is broken", method)
		}
	}
	return nil
}

func (e *environ) Name() string {
	return e.name
}

// GetImageBaseURLs returns a list of URLs which are used to search for simplestreams image metadata.
func (e *environ) GetImageBaseURLs() ([]string, error) {
	return []string{"dummy-image-metadata-url"}, nil
}

// GetToolsBaseURLs returns a list of URLs which are used to search for simplestreams tools metadata.
func (e *environ) GetToolsBaseURLs() ([]string, error) {
	return []string{"dummy-tools-url"}, nil
}

func (e *environ) Bootstrap(cons constraints.Value, possibleTools coretools.List, machineID string) error {
	defer delay()
	if err := e.checkBroken("Bootstrap"); err != nil {
		return err
	}
	password := e.Config().AdminSecret()
	if password == "" {
		return fmt.Errorf("admin-secret is required for bootstrap")
	}
	if _, ok := e.Config().CACert(); !ok {
		return fmt.Errorf("no CA certificate in environment configuration")
	}

	log.Infof("environs/dummy: would pick tools from %s", possibleTools)
	cfg, err := environs.BootstrapConfig(e.Config())
	if err != nil {
		return fmt.Errorf("cannot make bootstrap config: %v", err)
	}

	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	if estate.bootstrapped {
		return fmt.Errorf("environment is already bootstrapped")
	}
	if e.ecfg().stateServer() {
		// TODO(rog) factor out relevant code from cmd/jujud/bootstrap.go
		// so that we can call it here.

		info := stateInfo()
		st, err := state.Initialize(info, cfg, state.DefaultDialOpts())
		if err != nil {
			panic(err)
		}
		if err := st.SetEnvironConstraints(cons); err != nil {
			panic(err)
		}
		if err := st.SetAdminMongoPassword(utils.PasswordHash(password)); err != nil {
			panic(err)
		}
		_, err = st.AddUser("admin", password)
		if err != nil {
			panic(err)
		}
		estate.apiServer, err = apiserver.NewServer(st, "localhost:0", []byte(testing.ServerCert), []byte(testing.ServerKey))
		if err != nil {
			panic(err)
		}
		estate.apiState = st
	}
	estate.bootstrapped = true
	estate.ops <- OpBootstrap{Env: e.name, Constraints: cons}
	return nil
}

func (e *environ) StateInfo() (*state.Info, *api.Info, error) {
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	if err := e.checkBroken("StateInfo"); err != nil {
		return nil, nil, err
	}
	if !e.ecfg().stateServer() {
		return nil, nil, errors.New("dummy environment has no state configured")
	}
	if !estate.bootstrapped {
		return nil, nil, errors.New("dummy environment not bootstrapped")
	}
	return stateInfo(), &api.Info{
		Addrs:  []string{estate.apiServer.Addr()},
		CACert: []byte(testing.CACert),
	}, nil
}

func (e *environ) Config() *config.Config {
	return e.ecfg().Config
}

func (e *environ) SetConfig(cfg *config.Config) error {
	if err := e.checkBroken("SetConfig"); err != nil {
		return err
	}
	ecfg, err := providerInstance.newConfig(cfg)
	if err != nil {
		return err
	}
	e.ecfgMutex.Lock()
	e.ecfgUnlocked = ecfg
	e.ecfgMutex.Unlock()
	return nil
}

func (e *environ) Destroy([]instance.Instance) error {
	defer delay()
	if err := e.checkBroken("Destroy"); err != nil {
		return err
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	estate.ops <- OpDestroy{Env: estate.name}
	estate.destroy()
	return nil
}

// StartInstance is specified in the InstanceBroker interface.
func (e *environ) StartInstance(cons constraints.Value, possibleTools coretools.List,
	machineConfig *cloudinit.MachineConfig) (instance.Instance, *instance.HardwareCharacteristics, error) {

	defer delay()
	machineId := machineConfig.MachineId
	log.Infof("environs/dummy: dummy startinstance, machine %s", machineId)
	if err := e.checkBroken("StartInstance"); err != nil {
		return nil, nil, err
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	if machineConfig.MachineNonce == "" {
		return nil, nil, fmt.Errorf("cannot start instance: missing machine nonce")
	}
	if _, ok := e.Config().CACert(); !ok {
		return nil, nil, fmt.Errorf("no CA certificate in environment configuration")
	}
	if machineConfig.StateInfo.Tag != names.MachineTag(machineId) {
		return nil, nil, fmt.Errorf("entity tag must match started machine")
	}
	if machineConfig.APIInfo.Tag != names.MachineTag(machineId) {
		return nil, nil, fmt.Errorf("entity tag must match started machine")
	}
	log.Infof("environs/dummy: would pick tools from %s", possibleTools)
	series := possibleTools.OneSeries()
	i := &dummyInstance{
		id:           instance.Id(fmt.Sprintf("%s-%d", e.name, estate.maxId)),
		ports:        make(map[instance.Port]bool),
		machineId:    machineId,
		series:       series,
		firewallMode: e.Config().FirewallMode(),
		state:        estate,
	}
	var hc *instance.HardwareCharacteristics
	// To match current system capability, only provide hardware characteristics for
	// environ machines, not containers.
	if state.ParentId(machineId) == "" {
		// We will just assume the instance hardware characteristics exactly matches
		// the supplied constraints (if specified).
		hc = &instance.HardwareCharacteristics{
			Arch:     cons.Arch,
			Mem:      cons.Mem,
			RootDisk: cons.RootDisk,
			CpuCores: cons.CpuCores,
			CpuPower: cons.CpuPower,
		}
		// Fill in some expected instance hardware characteristics if constraints not specified.
		if hc.Arch == nil {
			arch := "amd64"
			hc.Arch = &arch
		}
		if hc.Mem == nil {
			mem := uint64(1024)
			hc.Mem = &mem
		}
		if hc.RootDisk == nil {
			disk := uint64(8192)
			hc.RootDisk = &disk
		}
		if hc.CpuCores == nil {
			cores := uint64(1)
			hc.CpuCores = &cores
		}
	}
	estate.insts[i.id] = i
	estate.maxId++
	estate.ops <- OpStartInstance{
		Env:          e.name,
		MachineId:    machineId,
		MachineNonce: machineConfig.MachineNonce,
		Constraints:  cons,
		Instance:     i,
		Info:         machineConfig.StateInfo,
		APIInfo:      machineConfig.APIInfo,
		Secret:       e.ecfg().secret(),
	}
	return i, hc, nil
}

func (e *environ) StopInstances(is []instance.Instance) error {
	defer delay()
	if err := e.checkBroken("StopInstance"); err != nil {
		return err
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	for _, i := range is {
		delete(estate.insts, i.(*dummyInstance).id)
	}
	estate.ops <- OpStopInstances{
		Env:       e.name,
		Instances: is,
	}
	return nil
}

func (e *environ) Instances(ids []instance.Id) (insts []instance.Instance, err error) {
	defer delay()
	if err := e.checkBroken("Instances"); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	notFound := 0
	for _, id := range ids {
		inst := estate.insts[id]
		if inst == nil {
			err = environs.ErrPartialInstances
			notFound++
		}
		insts = append(insts, inst)
	}
	if notFound == len(ids) {
		return nil, environs.ErrNoInstances
	}
	return
}

func (e *environ) AllInstances() ([]instance.Instance, error) {
	defer delay()
	if err := e.checkBroken("AllInstances"); err != nil {
		return nil, err
	}
	var insts []instance.Instance
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	for _, v := range estate.insts {
		insts = append(insts, v)
	}
	return insts, nil
}

func (e *environ) OpenPorts(ports []instance.Port) error {
	if mode := e.ecfg().FirewallMode(); mode != config.FwGlobal {
		return fmt.Errorf("invalid firewall mode %q for opening ports on environment", mode)
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	for _, p := range ports {
		estate.globalPorts[p] = true
	}
	return nil
}

func (e *environ) ClosePorts(ports []instance.Port) error {
	if mode := e.ecfg().FirewallMode(); mode != config.FwGlobal {
		return fmt.Errorf("invalid firewall mode %q for closing ports on environment", mode)
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	for _, p := range ports {
		delete(estate.globalPorts, p)
	}
	return nil
}

func (e *environ) Ports() (ports []instance.Port, err error) {
	if mode := e.ecfg().FirewallMode(); mode != config.FwGlobal {
		return nil, fmt.Errorf("invalid firewall mode %q for retrieving ports from environment", mode)
	}
	estate := e.state()
	estate.mu.Lock()
	defer estate.mu.Unlock()
	for p := range estate.globalPorts {
		ports = append(ports, p)
	}
	state.SortPorts(ports)
	return
}

func (*environ) Provider() environs.EnvironProvider {
	return &providerInstance
}

type dummyInstance struct {
	state        *environState
	ports        map[instance.Port]bool
	id           instance.Id
	machineId    string
	series       string
	firewallMode config.FirewallMode
}

func (inst *dummyInstance) Id() instance.Id {
	return inst.id
}

func (inst *dummyInstance) Status() string {
	return ""
}

func (inst *dummyInstance) DNSName() (string, error) {
	defer delay()
	return string(inst.id) + ".dns", nil
}

func (inst *dummyInstance) Addresses() ([]instance.Address, error) {
	log.Errorf("environs/dummy: Addresses not implemented")
	return nil, nil
}

func (inst *dummyInstance) WaitDNSName() (string, error) {
	return environs.WaitDNSName(inst)
}

func (inst *dummyInstance) OpenPorts(machineId string, ports []instance.Port) error {
	defer delay()
	log.Infof("environs/dummy: openPorts %s, %#v", machineId, ports)
	if inst.firewallMode != config.FwInstance {
		return fmt.Errorf("invalid firewall mode %q for opening ports on instance",
			inst.firewallMode)
	}
	if inst.machineId != machineId {
		panic(fmt.Errorf("OpenPorts with mismatched machine id, expected %q got %q", inst.machineId, machineId))
	}
	inst.state.mu.Lock()
	defer inst.state.mu.Unlock()
	inst.state.ops <- OpOpenPorts{
		Env:        inst.state.name,
		MachineId:  machineId,
		InstanceId: inst.Id(),
		Ports:      ports,
	}
	for _, p := range ports {
		inst.ports[p] = true
	}
	return nil
}

func (inst *dummyInstance) ClosePorts(machineId string, ports []instance.Port) error {
	defer delay()
	if inst.firewallMode != config.FwInstance {
		return fmt.Errorf("invalid firewall mode %q for closing ports on instance",
			inst.firewallMode)
	}
	if inst.machineId != machineId {
		panic(fmt.Errorf("ClosePorts with mismatched machine id, expected %s got %s", inst.machineId, machineId))
	}
	inst.state.mu.Lock()
	defer inst.state.mu.Unlock()
	inst.state.ops <- OpClosePorts{
		Env:        inst.state.name,
		MachineId:  machineId,
		InstanceId: inst.Id(),
		Ports:      ports,
	}
	for _, p := range ports {
		delete(inst.ports, p)
	}
	return nil
}

func (inst *dummyInstance) Ports(machineId string) (ports []instance.Port, err error) {
	defer delay()
	if inst.firewallMode != config.FwInstance {
		return nil, fmt.Errorf("invalid firewall mode %q for retrieving ports from instance",
			inst.firewallMode)
	}
	if inst.machineId != machineId {
		panic(fmt.Errorf("Ports with mismatched machine id, expected %q got %q", inst.machineId, machineId))
	}
	inst.state.mu.Lock()
	defer inst.state.mu.Unlock()
	for p := range inst.ports {
		ports = append(ports, p)
	}
	state.SortPorts(ports)
	return
}

// providerDelay controls the delay before dummy responds.
// non empty values in JUJU_DUMMY_DELAY will be parsed as
// time.Durations into this value.
var providerDelay time.Duration

// pause execution to simulate the latency of a real provider
func delay() {
	if providerDelay > 0 {
		log.Infof("environs/dummy: pausing for %v", providerDelay)
		<-time.After(providerDelay)
	}
}