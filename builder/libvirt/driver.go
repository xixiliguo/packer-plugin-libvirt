package libvirt

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/digitalocean/go-libvirt"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

type DriverCancelCallback func(state multistep.StateBag) bool

// A driver is able to talk to libvirt and perform certain
// operations with it.
type Driver interface {
	// Copy bypasses qemu-img convert and directly copies an image
	// that doesn't need converting.
	Copy(string, string) error

	// Stop stops a running machine, forcefully.
	Stop() error

	// Start starts domain of libvirt
	Start(Args ...string) error

	// wait on shutdown of the VM with option to cancel
	WaitForShutdown(<-chan struct{}) bool

	// Qemu executes the given command via qemu-img
	QemuImg(...string) error

	// Verify checks to make sure that this driver should function
	// properly. If there is any indication the driver can't function,
	// this will return an error.
	Verify() error

	// Version reads the version of Libvirt that is installed.
	Version() (string, error)
}

type LibvirtDriver struct {
	libvirt     *libvirt.Libvirt
	netBridge   string
	vmNet       libvirt.Network
	QemuImgPath string
	vmDomain    libvirt.Domain
	vmEndCh     <-chan int
	lock        sync.Mutex
}

func (d *LibvirtDriver) Stop() error {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.vmDomain.ID != 0 {
		if err := d.libvirt.DomainDestroy(d.vmDomain); err != nil {
			return err
		}
	}

	return nil
}

func (d *LibvirtDriver) Copy(sourceName, targetName string) error {
	source, err := os.Open(sourceName)
	if err != nil {
		err = fmt.Errorf("Error opening iso for copy: %s", err)
		return err
	}
	defer source.Close()

	// Create will truncate an existing file
	target, err := os.Create(targetName)
	if err != nil {
		err = fmt.Errorf("Error creating hard drive in output dir: %s", err)
		return err
	}
	defer target.Close()

	log.Printf("Copying %s to %s", source.Name(), target.Name())
	bytes, err := io.Copy(target, source)
	if err != nil {
		err = fmt.Errorf("Error copying iso to output dir: %s", err)
		return err
	}
	log.Printf(fmt.Sprintf("Copied %d bytes", bytes))

	return nil
}

func (d *LibvirtDriver) Start(Args ...string) error {

	XMLDesc := Args[0]

	log.Printf("Starting create domain from XML\n%s", XMLDesc)
	domain, err := d.libvirt.DomainCreateXML(XMLDesc, 0)
	if err != nil {
		return err
	}

	endCh := make(chan int, 1)
	// Setup our state so we know we are running
	d.vmEndCh = endCh
	d.vmDomain = domain

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		errorState := -1
		for range ticker.C {
			state, _, err := d.libvirt.DomainGetState(d.vmDomain, 0)
			if err != nil {
				log.Printf("Error getting domain state: %s", err)
				errorState = 99
			} else if libvirt.DomainState(state) != libvirt.DomainRunning {
				log.Printf("Domain state is %d", state)
				errorState = int(state)
			}
			if errorState != -1 {
				endCh <- errorState
				d.lock.Lock()
				d.vmDomain.ID = 0
				d.lock.Unlock()
				return
			}
		}
	}()
	return err
}

func (d *LibvirtDriver) GetDomainIP() (string, error) {
	ifaces, err := d.libvirt.DomainInterfaceAddresses(d.vmDomain, 0, 0)
	if err != nil {
		return "", nil
	}
	for _, iface := range ifaces {
		for _, addr := range iface.Addrs {
			if addr.Type == int32(libvirt.IPAddrTypeIpv4) {
				return addr.Addr, nil
			}
		}
	}
	return "", fmt.Errorf("No ipv4 address for domain %s", d.vmDomain.Name)
}

func (d *LibvirtDriver) WaitForShutdown(cancelCh <-chan struct{}) bool {
	d.lock.Lock()
	endCh := d.vmEndCh
	d.lock.Unlock()

	if endCh == nil {
		return true
	}

	select {
	case <-endCh:
		return true
	case <-cancelCh:
		return false
	}
}

func (d *LibvirtDriver) QemuImg(args ...string) error {
	var stdout, stderr bytes.Buffer

	log.Printf("Executing qemu-img: %#v", args)
	cmd := exec.Command(d.QemuImgPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	stdoutString := strings.TrimSpace(stdout.String())
	stderrString := strings.TrimSpace(stderr.String())

	if _, ok := err.(*exec.ExitError); ok {
		err = fmt.Errorf("QemuImg error: %s", stderrString)
	}

	log.Printf("stdout: %s", stdoutString)
	log.Printf("stderr: %s", stderrString)

	return err
}

func (d *LibvirtDriver) Verify() error {
	networks, _, err := d.libvirt.ConnectListAllNetworks(1, libvirt.ConnectListNetworksActive)
	if err != nil {
		return err
	}
	for _, network := range networks {
		bridge, err := d.libvirt.NetworkGetBridgeName(network)
		if err != nil {
			return err
		}
		if bridge == d.netBridge {
			d.vmNet = network
			return nil

		}
	}
	return fmt.Errorf("Not found available network for bridge %s", d.netBridge)
}

func (d *LibvirtDriver) Version() (string, error) {
	version, err := d.libvirt.Version()
	if err == nil {
		log.Printf("Libvirt version: %s", version)

	}
	return version, err
}

func logReader(name string, r io.Reader) {
	bufR := bufio.NewReader(r)
	for {
		line, err := bufR.ReadString('\n')
		if line != "" {
			line = strings.TrimRightFunc(line, unicode.IsSpace)
			log.Printf("%s: %s", name, line)
		}

		if err == io.EOF {
			break
		}
	}
}
