package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

var (
	Version string // version for this build, set at build time via LDFLAGS
	GitHash string // short-form hash of the commit of this build, set at build time
)

// Passing around config as a context to functions would be the ideomatic way.
// But we need to support configuration reload from signals and have that reload
// effect function calls in the main goroutine. Wherever possible we should be
// accessing via `getConfig` at the "top" of a goroutine and then use the config
// as context for a function after that.
var (
	globalConfig *Config
	configLock   = new(sync.RWMutex)
)

func getConfig() *Config {
	configLock.RLock()
	defer configLock.RUnlock()
	return globalConfig
}

type Config struct {
	Consul       string           `json:"consul,omitempty"`
	OnStart      json.RawMessage  `json:"onStart,omitempty"`
	PreStop      json.RawMessage  `json:"preStop,omitempty"`
	PostStop     json.RawMessage  `json:"postStop,omitempty"`
	StopTimeout  int              `json:"stopTimeout"`
	Services     []*ServiceConfig `json:"services"`
	Backends     []*BackendConfig `json:"backends"`
	onStartCmd   *exec.Cmd
	preStopCmd   *exec.Cmd
	postStopCmd  *exec.Cmd
	Command      *exec.Cmd
	QuitChannels []chan bool
}

type ServiceConfig struct {
	Id               string
	Name             string          `json:"name"`
	Poll             int             `json:"poll"` // time in seconds
	HealthCheckExec  json.RawMessage `json:"health"`
	Port             int             `json:"port"`
	TTL              int             `json:"ttl"`
	Interfaces       json.RawMessage `json:"interfaces"`
	discoveryService DiscoveryService
	ipAddress        string
	healthCheckCmd   *exec.Cmd
}

type BackendConfig struct {
	Name             string          `json:"name"`
	Poll             int             `json:"poll"` // time in seconds
	OnChangeExec     json.RawMessage `json:"onChange"`
	discoveryService DiscoveryService
	lastState        interface{}
	onChangeCmd      *exec.Cmd
}

type Pollable interface {
	PollTime() int
}

func (b BackendConfig) PollTime() int {
	return b.Poll
}
func (b *BackendConfig) CheckForUpstreamChanges() bool {
	return b.discoveryService.CheckForUpstreamChanges(b)
}

func (b *BackendConfig) OnChange() (int, error) {
	exitCode, err := run(b.onChangeCmd)
	// Reset command object - since it can't be reused
	b.onChangeCmd = argsToCmd(b.onChangeCmd.Args)
	return exitCode, err
}

func (s ServiceConfig) PollTime() int {
	return s.Poll
}
func (s *ServiceConfig) SendHeartbeat() {
	s.discoveryService.SendHeartbeat(s)
}

func (s *ServiceConfig) MarkForMaintenance() {
	s.discoveryService.MarkForMaintenance(s)
}

func (s *ServiceConfig) Deregister() {
	s.discoveryService.Deregister(s)
}

func (s *ServiceConfig) CheckHealth() (int, error) {
	exitCode, err := run(s.healthCheckCmd)
	// Reset command object - since it can't be reused
	s.healthCheckCmd = argsToCmd(s.healthCheckCmd.Args)
	return exitCode, err
}

const (
	// Amount of time to wait before killing the application
	defaultStopTimeout int = 5
)

func parseInterfaces(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return []string{}, nil
	}
	// Parse as a string
	var jsonString string
	if err := json.Unmarshal(raw, &jsonString); err == nil {
		return []string{jsonString}, nil
	}

	var jsonArray []string
	if err := json.Unmarshal(raw, &jsonArray); err == nil {
		return jsonArray, nil
	}

	return []string{}, errors.New("interfaces must be a string or an array")
}

func parseCommandArgs(raw json.RawMessage) (*exec.Cmd, error) {
	if raw == nil {
		return nil, nil
	}
	// Parse as a string
	var stringCmd string
	if err := json.Unmarshal(raw, &stringCmd); err == nil {
		return strToCmd(stringCmd), nil
	}

	var arrayCmd []string
	if err := json.Unmarshal(raw, &arrayCmd); err == nil {
		return argsToCmd(arrayCmd), nil
	}
	return nil, errors.New("Command argument must be a string or an array")
}

func loadConfig() (*Config, error) {

	var configFlag string
	var versionFlag bool

	if !flag.Parsed() {
		flag.StringVar(&configFlag, "config", "",
			"JSON config or file:// path to JSON config file.")
		flag.BoolVar(&versionFlag, "version", false, "Show version identifier and quit.")
		flag.Parse()
	} else {
		// allows for safe configuration reload
		configFlag = flag.Lookup("config").Value.String()
	}
	if versionFlag {
		fmt.Printf("Version: %s\nGitHash: %s\n", Version, GitHash)
		os.Exit(0)
	}
	if configFlag == "" {
		configFlag = os.Getenv("CONTAINERBUDDY")
	}

	config, err := parseConfig(configFlag)
	if err != nil {
		return nil, err
	}
	return initializeConfig(config)
}

func initializeConfig(config *Config) (*Config, error) {
	var discovery DiscoveryService
	discoveryCount := 0
	onStartCmd, err := parseCommandArgs(config.OnStart)
	if err != nil {
		return nil, fmt.Errorf("Could not parse `onStart`: %s", err)
	}
	config.onStartCmd = onStartCmd

	preStopCmd, err := parseCommandArgs(config.PreStop)
	if err != nil {
		return nil, fmt.Errorf("Could not parse `preStop`: %s", err)
	}
	config.preStopCmd = preStopCmd

	postStopCmd, err := parseCommandArgs(config.PostStop)
	if err != nil {
		return nil, fmt.Errorf("Could not parse `postStop`: %s", err)
	}
	config.postStopCmd = postStopCmd

	for _, discoveryBackend := range []string{"Consul"} {
		switch discoveryBackend {
		case "Consul":
			if config.Consul != "" {
				discovery = NewConsulConfig(config.Consul)
				discoveryCount += 1
			}
		}
	}

	if discoveryCount == 0 {
		return nil, errors.New("No discovery backend defined")
	} else if discoveryCount > 1 {
		return nil, errors.New("More than one discovery backend defined")
	}

	if config.StopTimeout == 0 {
		config.StopTimeout = defaultStopTimeout
	}

	for _, backend := range config.Backends {
		if backend.Name == "" {
			return nil, fmt.Errorf("backend must have a `name`")
		}
		cmd, err := parseCommandArgs(backend.OnChangeExec)
		if err != nil {
			return nil, fmt.Errorf("Could not parse `onChange` in backend %s: %s",
				backend.Name, err)
		}
		if cmd == nil {
			return nil, fmt.Errorf("`onChange` is required in backend %s",
				backend.Name)
		}
		if backend.Poll < 1 {
			return nil, fmt.Errorf("`poll` must be > 0 in backend %s",
				backend.Name)
		}
		backend.onChangeCmd = cmd
		backend.discoveryService = discovery
	}

	hostname, _ := os.Hostname()
	for _, service := range config.Services {
		if service.Name == "" {
			return nil, fmt.Errorf("service must have a `name`")
		}
		service.Id = fmt.Sprintf("%s-%s", service.Name, hostname)
		service.discoveryService = discovery
		if service.Poll < 1 {
			return nil, fmt.Errorf("`poll` must be > 0 in service %s",
				service.Name)
		}
		if service.TTL < 1 {
			return nil, fmt.Errorf("`ttl` must be > 0 in service %s",
				service.Name)
		}
		if service.Port < 1 {
			return nil, fmt.Errorf("`port` must be > 0 in service %s",
				service.Name)
		}

		if cmd, err := parseCommandArgs(service.HealthCheckExec); err != nil {
			return nil, fmt.Errorf("Could not parse `health` in service %s: %s",
				service.Name, err)
		} else if cmd == nil {
			return nil, fmt.Errorf("`health` is required in service %s",
				service.Name)
		} else {
			service.healthCheckCmd = cmd
		}

		interfaces, ifaceErr := parseInterfaces(service.Interfaces)
		if ifaceErr != nil {
			return nil, ifaceErr
		}

		if service.ipAddress, err = getIp(interfaces); err != nil {
			return nil, err
		}
	}

	configLock.Lock()
	globalConfig = config
	configLock.Unlock()

	return config, nil
}

func parseConfig(configFlag string) (*Config, error) {
	if configFlag == "" {
		return nil, errors.New("-config flag is required.")
	}

	var data []byte
	if strings.HasPrefix(configFlag, "file://") {
		var err error
		fName := strings.SplitAfter(configFlag, "file://")[1]
		if data, err = ioutil.ReadFile(fName); err != nil {
			return nil, fmt.Errorf("Could not read config file: %s", err)
		}
	} else {
		data = []byte(configFlag)
	}

	if template, err := ApplyTemplate(data); err != nil {
		return nil, fmt.Errorf(
			"Could not apply template to config: %s", err)
	} else {
		data = template
	}

	return unmarshalConfig(data)
}

func unmarshalConfig(data []byte) (*Config, error) {
	config := &Config{}
	if err := json.Unmarshal(data, &config); err != nil {
		syntax, ok := err.(*json.SyntaxError)
		if !ok {
			return nil, fmt.Errorf(
				"Could not parse configuration: %s",
				err)
		}
		return nil, newJSONParseError(data, syntax)
	}
	return config, nil
}

func newJSONParseError(js []byte, syntax *json.SyntaxError) error {
	line, col, err := highlightError(js, syntax.Offset)
	return fmt.Errorf("Parse error at line:col [%d:%d]: %s\n%s", line, col, syntax, err)
}

func highlightError(data []byte, pos int64) (int, int, string) {
	prevLine := ""
	thisLine := ""
	highlight := ""
	line := 1
	col := pos
	offset := int64(0)
	r := bytes.NewReader(data)
	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		prevLine = thisLine
		thisLine = fmt.Sprintf("%5d: %s\n", line, scanner.Text())
		readBytes := int64(len(scanner.Bytes()))
		offset += readBytes
		if offset >= pos-1 {
			highlight = fmt.Sprintf("%s^", strings.Repeat("-", int(7+col-1)))
			break
		}
		col -= readBytes + 1
		line++
	}
	return line, int(col), fmt.Sprintf("%s%s%s", prevLine, thisLine, highlight)
}

// determine the IP address of the container
func getIp(interfaceNames []string) (string, error) {

	if interfaceNames == nil || len(interfaceNames) == 0 {
		// Use a sane default
		interfaceNames = []string{"eth0"}
	}

	interfaces, interfacesErr := net.Interfaces()

	if interfacesErr != nil {
		return "", interfacesErr
	}

	interfaceIps, interfaceIpsErr := getInterfaceIps(interfaces)

	/* We had an error and there were no interfaces returned, this is clearly
	 * an error state. */
	if interfaceIpsErr != nil && len(interfaceIps) < 1 {
		return "", interfaceIpsErr
	}
	/* We had error(s) and there were interfaces returned, this is potentially
	 * recoverable. Let's pass on the parsed interfaces and log the error
	 * state. */
	if interfaceIpsErr != nil && len(interfaceIps) > 0 {
		log.Printf("We had a problem reading information about some network "+
			"interfaces. If everything works, it is safe to ignore this"+
			"message. Details:\n%s\n", interfaceIpsErr)
	}

	// Find the interface matching the name given
	for _, interfaceName := range interfaceNames {
		for _, intf := range interfaceIps {
			if interfaceName == intf.Name {
				return intf.IP, nil
			}
		}
	}

	// Interface not found, return error
	return "", errors.New(fmt.Sprintf("Unable to find interfaces %s in %#v",
		interfaceNames, interfaceIps))
}

type InterfaceIp struct {
	Name string
	IP   string
}

// Queries the network interfaces on the running machine and returns a list
// of IPs for each interface. Currently, this only returns IPv4 addresses.
func getInterfaceIps(interfaces []net.Interface) ([]InterfaceIp, error) {
	var ifaceIps []InterfaceIp
	var errors []string

	for _, intf := range interfaces {
		ipAddrs, addrErr := intf.Addrs()

		if addrErr != nil {
			errors = append(errors, addrErr.Error())
			continue
		}

		/* As crazy as it may seem, yes you can have an interface that doesn't
		 * have an IP address assigned. */
		if len(ipAddrs) == 0 {
			continue
		}

		/* We ignore aliases for the time being. We assume that that
		 * authoritative address is the first address returned from the
		 * interface. */
		ifaceIp, parsingErr := parseIpFromAddress(ipAddrs[0], intf)

		if parsingErr != nil {
			errors = append(errors, parsingErr.Error())
			continue
		}

		ifaceIps = append(ifaceIps, ifaceIp)
	}

	/* If we had any errors parsing interfaces, we accumulate them all and
	 * then return them so that the caller can decide what they want to do. */
	if len(errors) > 0 {
		err := fmt.Errorf(strings.Join(errors, "\n"))
		println(err.Error())
		return ifaceIps, err
	}

	return ifaceIps, nil
}

// Parses an IP and interface name out of the provided address and interface
// objects. We assume that the default IPv4 address will be the first IPv4 address
// to appear in the list of IPs presented for the interface.
func parseIpFromAddress(address net.Addr, intf net.Interface) (InterfaceIp, error) {
	ips := strings.Split(address.String(), " ")

	// In Linux, we will typically see a value like:
	// 192.168.0.7/24 fe80::12c3:7bff:fe45:a2ff/64

	var ipv4 string
	ipv4Regex := "^\\d+\\.\\d+\\.\\d+\\.\\d+.*$"

	for _, ip := range ips {
		matched, matchErr := regexp.MatchString(ipv4Regex, ip)

		if matchErr != nil {
			return InterfaceIp{}, matchErr
		}

		if matched {
			ipv4 = ip
			break
		}
	}

	if len(ipv4) < 1 {
		msg := fmt.Sprintf("No parsable IPv4 address was available for "+
			"interface: %s", intf.Name)
		return InterfaceIp{}, errors.New(msg)
	}

	ipAddr, _, parseErr := net.ParseCIDR(ipv4)

	if parseErr != nil {
		return InterfaceIp{}, parseErr
	}

	ifaceIp := InterfaceIp{Name: intf.Name, IP: ipAddr.String()}

	return ifaceIp, nil
}

func argsToCmd(args []string) *exec.Cmd {
	if len(args) == 0 {
		return nil
	}
	if len(args) > 1 {
		return exec.Command(args[0], args[1:]...)
	} else {
		return exec.Command(args[0])
	}
}

func strToCmd(command string) *exec.Cmd {
	if command != "" {
		return argsToCmd(strings.Split(strings.TrimSpace(command), " "))
	}
	return nil
}
