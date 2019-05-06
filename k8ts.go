package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akamensky/argparse"
	"github.com/alessio/shellescape"
	"github.com/appleboy/easyssh-proxy"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"
	"net"
	"net/url"
)

const remoteInstallPath string = "/usr/bin"
const remoteUploadPath string = "/tmp"
const binaryName string = "k8ts"
const kubernetesLogsPath string = "/var/log/containers"
const tombstonePath string = "/var/log/tombstone"
const systemdUnitsPath = "/etc/systemd/system"

func deploy(target *SshHost, proxy *SshHost, args *MonitorArgs) error {
	tagetSSH := &easyssh.MakeConfig{
		User:     target.user,
		Password: target.password,
		Server:   target.host,
		Port:     target.port,
		Timeout:  60 * time.Second,
	}
	if target.keyPath != "" {
		tagetSSH.KeyPath = target.keyPath
	}
	if proxy != nil {
		proxySSH := easyssh.DefaultConfig{
			User: proxy.user,
			Password: proxy.password,
			Server : proxy.host,
			Port: proxy.port,
		}
		if proxy.keyPath != "" {
			proxySSH.KeyPath = proxy.keyPath
		}
		tagetSSH.Proxy = proxySSH
	}
	uploadPath := filepath.Join(remoteUploadPath, binaryName)
	_, _, _, _ = tagetSSH.Run(fmt.Sprintf("rm -f " + uploadPath))
	err := tagetSSH.Scp(os.Args[0], uploadPath)
	if err != nil {
		fmt.Printf("Upload to '%s' failed.", uploadPath)
		return err
	}
	_, _, _, err = tagetSSH.Run("chmod a+x " + uploadPath)
	if err != nil {
		fmt.Printf("Failed to mark '%s' executable\n", uploadPath)
		return err
	}
	installPath := filepath.Join(remoteInstallPath, binaryName)
	_, _, _, err = tagetSSH.Run("sudo mv " + uploadPath + " " + installPath)
	if err != nil {
		fmt.Printf("Failed to install '%s'\n", installPath)
		return err
	}
	fmt.Println("Deploy successful. (re)Install service")
	_, _, _, _ = tagetSSH.Run("sudo " + installPath + " service uninstall")
	_, _, _, _ = tagetSSH.Run("sudo " + installPath + " service install " + args.String())
	return nil
}

func openFile(name string) (*os.File, error) {
	filePath := filepath.Join(kubernetesLogsPath, name)
	for {
		stat, err := os.Stat(filePath)
		if err != nil {
			log.Printf("Stat failed for path %s. Reason: %v\n", filePath, err)
			return nil, err
		}
		if (stat.Mode() & os.ModeSymlink) != os.ModeSymlink {
			break
		}
		newPath, err := os.Readlink(filePath)
		if err != nil {
			log.Printf("Unable to read link %s. Reason: %v\n", filePath, err)
			break
		}
		if newPath == filePath {
			break
		}
		filePath = newPath
	}

	return os.Open(filePath)
}

const serviceUnitTemplate string = `
[Unit]
Description=Preserve logs of Kubernetes pods and jobs
Requires=kubelet.service

[Service]
Type=simple
ExecStart=%s monitor %s
Restart=always

[Install]
WantedBy=default.target
`

func serviceInstall(args *MonitorArgs) error {
	unitPath := filepath.Join(systemdUnitsPath, binaryName + ".service")
	unitFile, err := os.OpenFile(unitPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open '%s'", unitPath)
		return err
	}
	_, _ = fmt.Fprintf(unitFile, serviceUnitTemplate,
		filepath.Join(remoteInstallPath, binaryName),
		args.String())
	cmd := exec.Command("systemctl", "daemon-reload")
	err = cmd.Run()
	if err != nil {
		log.Printf("Failed to run command %v\n", cmd)
		return err
	}
	cmd = exec.Command("systemctl", "enable", "k8ts")
	err = cmd.Run()
	if err != nil {
		log.Printf("Failed to run command %v\n", cmd)
		return err
	}
	cmd = exec.Command("systemctl", "start", "k8ts")
	err = cmd.Run()
	if err != nil {
		log.Printf("Failed to run command %v\n", cmd)
		return err
	}
	return nil
}

func serviceUninstall() error {
	cmd := exec.Command("sudo", "systemctl", "stop", binaryName)
	_ = cmd.Run()
	cmd = exec.Command("sudo", "systemctl", "disable", binaryName)
	_ = cmd.Run()
	unitPath := filepath.Join(systemdUnitsPath, binaryName + ".service")
	_ = os.Remove(unitPath)
	return nil
}

type monitor struct {
	includePattern   *regexp.Regexp
	excludePattern   *regexp.Regexp
	keepIf         *regexp.Regexp
	skipConversion bool
	monitoredFiles map[string](*os.File)
}

func (m *monitor) skip(fileName string) bool {
	skipFile := false
	if m.includePattern != nil && !m.includePattern.MatchString(fileName) {
		log.Printf("Event: not in the included mask. Skip it")
		skipFile = true
	}
	if m.excludePattern != nil && m.excludePattern.MatchString(fileName) {
		log.Printf("Event: matches exclude mask. Skip it")
		skipFile = true
	}
	return skipFile
}

func (m *monitor) watch(fileName string) {
	if m.skip(fileName) {
		return
	}
	file, err := openFile(fileName)
	if err != nil {
		log.Printf("Failed to open file %s\n", fileName)
	} else {
		m.monitoredFiles[fileName] = file
	}
}

func (m *monitor) unwatch(fileName string) {
	source, ok := m.monitoredFiles[fileName]
	if !ok {
		log.Printf("Unregistered file '%s' gone forever\n", fileName)
		return
	}
	defer delete(m.monitoredFiles, fileName)
	defer func(){ _ = source.Close() }()
	if m.keepIf != nil {
		_, err := source.Seek(0, io.SeekStart)
		if err != nil {
			log.Println("Seek failed")
			return
		}
		if !search(source, m.keepIf) {
			log.Printf("File '%s' does not match keep-if pattern. Skip it", fileName)
		}
	}
	filePath := filepath.Join(tombstonePath, fileName)
	destination, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open tombstone for '%s'. Reason: %v\n", fileName, err)
		return
	}
	defer func(){ _ = destination.Close() }()
	_, err = source.Seek(0, io.SeekStart)
	if err != nil {
		log.Println("Seek failed")
		return
	}
	if m.skipConversion {
		err = passThrough(destination, source)
	} else {
		err = jsonToText(destination, source)
	}
	if err != nil {
		log.Printf("Failed to copy file data for '%s'. Reason: %v\n", fileName, err)
	} else {
		log.Printf("Created tombstone for %s\n", fileName)
	}
}

func passThrough(destination io.Writer, source io.Reader) error {
	_, err := io.Copy(destination, source)
	return err
}

func search(source io.Reader, pattern *regexp.Regexp) bool {
	scanner := bufio.NewScanner(source)
	for scanner.Scan() {
		if pattern.Find(scanner.Bytes()) != nil {
			return true
		}
	}
	return false
}

type logEntry struct {
	Log    string
	Stream string
	Time   string
}

func jsonToText(destination io.Writer, source io.Reader) error {
	scanner := bufio.NewScanner(source)
	for scanner.Scan() {
		message := logEntry{}
		line := scanner.Bytes()
		err := json.Unmarshal(line, &message)
		if err != nil {
			log.Printf("Failed to unpack log entry '%s'", string(line))
			return err
		}
		_, err = io.WriteString(destination, message.Time)
		if err != nil {
			log.Printf("Write failed")
			return err
		}
		_, err = destination.Write([]byte{' '})
		if err != nil {
			log.Printf("Write failed")
			return err
		}
		_, err = io.WriteString(destination, message.Stream)
		if err != nil {
			log.Printf("Write failed")
			return err
		}
		_, err = destination.Write([]byte{' '})
		if err != nil {
			log.Printf("Write failed")
			return err
		}
		_, err = io.WriteString(destination, message.Log)
		if err != nil {
			log.Printf("Write failed")
			return err
		}
		if !strings.HasSuffix(message.Log, "\n") {
			_, err = destination.Write([]byte{'\n'})
			if err != nil {
				log.Printf("Write failed")
				return err
			}
		}
	}
	return nil
}

func newMonitor(args *MonitorArgs) *monitor {
	var includePattern *regexp.Regexp
	if *args.includeLog != "" {
		includePattern = regexp.MustCompile(*args.includeLog)
	}
	var excludePattern *regexp.Regexp
	if *args.excludeLog != "" {
		excludePattern = regexp.MustCompile(*args.excludeLog)
	}
	var keepIf *regexp.Regexp
	if *args.keepIf != "" {
		keepIf = regexp.MustCompile(*args.keepIf)
	}
	return &monitor{includePattern, excludePattern, keepIf,
		*args.skipConversion, make(map[string](*os.File))}
}

func (m *monitor) run() error {
	fd, err := syscall.InotifyInit()
	if err != nil {
		return err
	}
	inotify := os.NewFile(uintptr(fd), "inotify")
	defer func(){ _ = inotify.Close() }()

	const maxEventSize int = syscall.SizeofInotifyEvent + syscall.NAME_MAX + 1
	eventBuffer := make([]byte, maxEventSize * 20)

	err = os.MkdirAll(tombstonePath, 0755)
	if err != nil {
		log.Fatal(err)
	}

	_, err = syscall.InotifyAddWatch(
		fd, kubernetesLogsPath,
		syscall.IN_CREATE|syscall.IN_DELETE)
	if err != nil {
		log.Fatal(err)
	}

	var bytesLeft uint32 = 0
	for {
		readCount, err := inotify.Read(eventBuffer[bytesLeft:])
		if err != nil {
			log.Fatal(err)
		}
		bytesAvailable := bytesLeft + uint32(readCount)
		if bytesAvailable < syscall.SizeofInotifyEvent {
			log.Printf("Short read. Expecting %d bytes. Got %d instead.\n",
				syscall.SizeofInotifyEvent, readCount)
			continue
		}
		var offset uint32
		for offset <= uint32(readCount-syscall.SizeofInotifyEvent) {
			eventSize := handleEvent(eventBuffer, bytesAvailable, offset, m)
			offset += syscall.SizeofInotifyEvent + eventSize
		}
	}
}

func handleEvent(eventBuffer []byte, bytesAvailable uint32, offset uint32, m *monitor) uint32 {
	rawEvent := (*syscall.InotifyEvent)(unsafe.Pointer(&eventBuffer[offset]))
	eventSize := syscall.SizeofInotifyEvent + rawEvent.Len
	if (offset + eventSize) > uint32(bytesAvailable) {
		bytesLeft := uint32(bytesAvailable) - offset
		copy(eventBuffer[0:bytesLeft], eventBuffer[offset:bytesAvailable])
	}
	nameBytes := (*[syscall.NAME_MAX]byte)(unsafe.Pointer(&rawEvent.Name))[0:rawEvent.Len]
	name := strings.TrimRight(string(nameBytes), "\0000")
	log.Printf("Event: mask=%x, name=%s\n", rawEvent.Mask, name)
	if (rawEvent.Mask & syscall.IN_CREATE) == syscall.IN_CREATE {
		m.watch(name)
	} else if (rawEvent.Mask & syscall.IN_DELETE) == syscall.IN_DELETE {
		m.unwatch(name)
	} else {
		log.Printf("Unsupported event mask %x for %s\n", rawEvent.Mask, name)
	}
	return rawEvent.Len
}

type ParserAction func() error

type MonitorArgs struct {
	includeLog     *string
	excludeLog     *string
	keepIf         *string
	skipConversion *bool
}

type DeployArgs struct {
	target  *string
	targetKey  *string
	proxy   *string
	proxyKey   *string
	monitor *MonitorArgs
}

type SshHost struct {
	user string
	password string
	host string
	port string
	keyPath string
}

func NewSshHost(host string, keyPath string) (*SshHost, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		fmt.Printf("Invalid host/port '%s'", u.Host)
		return nil, err
	}
	password, ok := u.User.Password()
	if !ok {
		password = ""
	}
	return &SshHost{
		user: u.User.Username(),
		password: password,
		host: host,
		port: port,
		keyPath: keyPath,
	}, nil
}

type ServiceInstallArgs struct {
	command *argparse.Command
	monitor *MonitorArgs
}

type ServiceArgs struct {
	install   ServiceInstallArgs
	uninstall *argparse.Command
}

func (args *MonitorArgs) String() string {
	var out strings.Builder
	if args.includeLog != nil && *args.includeLog != "" {
		fmt.Fprintf(&out, "--include-log %s",
			shellescape.Quote(*args.includeLog))
	}
	if args.excludeLog != nil && *args.excludeLog != "" {
		if out.Len() > 0 {
			fmt.Fprint(&out, " ")
		}
		fmt.Fprintf(&out, "--exclude-log %s",
			shellescape.Quote(*args.includeLog))
	}
	if args.keepIf != nil && *args.keepIf != "" {
		if out.Len() > 0 {
			fmt.Fprint(&out, " ")
		}
		fmt.Fprintf(&out, "--keep-if %s",
			shellescape.Quote(*args.includeLog))
	}
	return out.String()
}

func parseArgs() int {
	parser := argparse.NewParser("k8ts", "k8ts ... because some pods need to be remembered")

	attachMonitorArgs := func(cmd *argparse.Command) *MonitorArgs {
		return &MonitorArgs{
			includeLog: cmd.String("i", "include-log",
				&argparse.Options{Help: "Preserve logs of pods matching this pattern.", Required: false}),
			excludeLog: cmd.String("e", "exclude-log",
				&argparse.Options{Help: "Ignore logs of pods matching this pattern.", Required: false}),
			keepIf: cmd.String("k", "keep-if",
				&argparse.Options{Help: "Keep logs only if content matches this pattern.", Required: false}),
			skipConversion: cmd.Flag("s", "skip-conversion",
				&argparse.Options{Help: "Do not convert logs from JSON to text.", Required: false}),
		}
	}

	deployCmd := parser.NewCommand("deploy", "Deploy k8ts on a remote host via SSH")
	deployArgs := DeployArgs{
		target: deployCmd.String("t", "target",
			&argparse.Options{Help: "Where to deploy k8ts", Required: true}),
		targetKey: deployCmd.String("k", "target-key",
			&argparse.Options{Help: "SSH key to use when connecting to taget", Required: false}),
		proxy: deployCmd.String("p", "proxy",
			&argparse.Options{Help: "Next hop (proxy) used to reach target host", Required: false}),
		proxyKey: deployCmd.String("q", "proxy-key",
			&argparse.Options{Help: "SSH key to use when connecting to proxy", Required: false}),
		monitor: attachMonitorArgs(deployCmd),
	}

	serviceCmd := parser.NewCommand("service", "Control k8ts service running on this host")
	serviceArgs := ServiceArgs{
		install: ServiceInstallArgs{
			command: serviceCmd.NewCommand("install", "Install service"),
			monitor: attachMonitorArgs(serviceCmd),
		},
		uninstall: serviceCmd.NewCommand("uninstall", "Uninstall service"),
	}

	monitorCmd := parser.NewCommand("monitor", "Monitor kubernetes pod logs")
	monitorArgs := attachMonitorArgs(monitorCmd)

	err := parser.Parse(os.Args)
	if err != nil {
		fmt.Print(parser.Usage(err))
		return 1
	}

	var action ParserAction = func() error {
		fmt.Println("No command selected.")
		fmt.Println(parser.Usage(err))
		return errors.New("no-command")
	}
	if deployCmd.Happened() {
		action = func() error {
			target, err := NewSshHost("ssh://" + *deployArgs.target, *deployArgs.targetKey)
			if err != nil {
				fmt.Printf("Invalid SSH target '%s'", *deployArgs.target)
				return err
			}
			var proxy *SshHost
			if *deployArgs.proxy != "" {
				proxy, err = NewSshHost("ssh://" + *deployArgs.target, *deployArgs.proxyKey)
				if err != nil {
					fmt.Printf("Invalid SSH proxy '%s'", *deployArgs.target)
					return err
				}
			}
			if err != nil {
				fmt.Printf("Invalid target '%s'\n", *deployArgs.target)
				return err
			}
			return deploy(target, proxy, deployArgs.monitor)
		}
	} else if serviceCmd.Happened() {
		if serviceArgs.install.command.Happened() {
			action = func() error {
				return serviceInstall(serviceArgs.install.monitor)
			}
		} else if serviceArgs.uninstall.Happened() {
			action = serviceUninstall
		}
	} else if monitorCmd.Happened() {
		action = func() error {
			return newMonitor(monitorArgs).run()
		}
	}
	err = action()
	if err != nil {
		log.Fatal(err)
	}
	return 0
}

func main() {
	os.Exit(parseArgs())
}
