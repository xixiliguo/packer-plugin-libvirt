package libvirt

import (
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

// stepRun runs the virtual machine
type stepRun struct {
	DiskImage bool

	atLeastVersion2 bool
	ui              packersdk.Ui
}

type XMLTemplateData struct {
	VncIP       string
	VncPort     int
	VncPassword string
	VMName      string
	Disks       []Disk
	IsoPath     string
}

var XmlTemplate string = `<domain type='{{.Hypervisor}}'>
	<name>{{.Name}}</name>
	<vcpu>{{.Vcpu}}</vcpu>
	<memory unit='MiB'>{{.Memory}}</memory>
	<os>
		<type arch='{{.Arch}}' machine='{{.Machine}}'>hvm</type>
		{{if .Loader}}<loader readonly='yes' type='pflash'>{{.Loader}}</loader>{{end}}
		<boot dev='hd'/>
		<boot dev='cdrom'/>
	</os>
	<features>
		<acpi/>
		<apic/>
	</features>
	{{if eq .CPUMode "host-passthrough" "host-model"}}
	<cpu mode='{{.CPUMode}}'>
	</cpu>
	{{else}}
	<cpu mode='custom' match='exact' check='none'>
    	<model fallback='allow'>{{.CPUMode}}</model>
  	</cpu>
	{{end}}
	<clock offset='utc'>
	</clock>
	<on_poweroff>destroy</on_poweroff>
	<on_reboot>restart</on_reboot>
	<on_crash>destroy</on_crash>
	<devices>
	<emulator>{{.Emulator}}</emulator>
	{{range .Disks}}
	<disk type='file' device='disk'>
		<driver name='qemu' type='{{.Format}}' cache='{{.DiskCache}}' discard='{{.DiskDiscard}}' {{if ne .DetectZeroes "off"}}detect_zeroes='{{.DetectZeroes}}'{{end}}/>
		<source file='{{.Source}}'/>
		<target dev='{{.Dev}}' bus='{{.DiskInterface}}'/>
	</disk>
	{{end}}
	{{if not .DiskImage}}
	<disk type='file' device='cdrom'>
		<driver name='qemu' type='raw'/>
		<source file='{{.Cdrom.Source}}'/>
		<target dev='sdd'/>
		<readonly/>
	</disk>
	{{end}}
	{{if .FloppyPath}}
	<disk type='file' device='floppy'>
		<driver name='qemu' type='raw'/>
		<source file='{{.FloppyPath}}'/>
		<target dev='fda'/>
		<readonly/>
	</disk>
	{{end}}
	<controller type='usb' index='0' model='ehci'>
		<address type='pci' domain='0x0000' bus='0x02' slot='0x01' function='0x0'/>
	</controller>
	<controller type='scsi' index='0' model='virtio-scsi'>
		<address type='pci' domain='0x0000' bus='0x02' slot='0x02' function='0x0'/>
	</controller>
	<interface type='network'>
		<source network='{{.NetName}}'/>
		<model type='{{.NetDevice}}'/>
	</interface>
	<serial type='pty'>
		<source path='/dev/pts/0'/>
		<target type='{{if eq .Arch "x86_64"}}isa-serial{{else}}system-serial{{end}}' port='0'/>
	</serial>
	<console type='pty' tty='/dev/pts/0'>
		<source path='/dev/pts/0'/>
		<target type='serial' port='0'/>
	</console>
	<input type='tablet'>
		<alias name='input0'/>
	</input>
	<input type='keyboard'>
		<alias name='input1'/>
	</input>
	<graphics type='vnc' port='{{.VncPort}}' {{if .VncPassword}}passwd='{{.VncPassword}}'{{end}}>
		<listen type='address' address='{{.VncIP}}'/>
	</graphics>
	<video>
		<model type='{{if eq .Arch "x86_64"}}cirrus{{else}}virtio{{end}}' primary='yes'/>
	</video>
	<memballoon model='virtio'>
		<address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
	</memballoon>
	</devices>	
</domain>
`

type LibvirtXML struct {
	Hypervisor  string
	Name        string
	Vcpu        int
	Memory      int
	Loader      string
	Arch        string
	Machine     string
	CPUMode     string
	Emulator    string
	DiskImage   bool
	Disks       []Disk
	Cdrom       Cdrom
	FloppyPath  string
	NetName     string
	NetDevice   string
	VncIP       string
	VncPort     int
	VncPassword string
}

type Disk struct {
	Format        string
	Source        string
	Dev           string
	DiskCache     string
	DiskDiscard   string
	DetectZeroes  string
	DiskInterface string
}

type Cdrom struct {
	Format string
	Source string
}

func (s *stepRun) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	driver := state.Get("driver").(Driver)
	s.ui = state.Get("ui").(packersdk.Ui)

	message := "Starting VM, booting from CD-ROM"
	if config.DiskImage {
		message = "Starting VM, booting disk image"
	}
	s.ui.Say(message)

	// Generate xml
	xmlDesc, err := s.getXMLDesc(state)
	if err != nil {
		err := fmt.Errorf("Error generating XML: %s", err)
		state.Put("error", err)
		s.ui.Error(err.Error())
		return multistep.ActionHalt
	}
	// run libvirt
	if err := driver.Start(xmlDesc); err != nil {
		err := fmt.Errorf("Error starting VM: %s", err)
		state.Put("error", err)
		s.ui.Error(err.Error())
		return multistep.ActionHalt
	}

	return multistep.ActionContinue
}

func (s *stepRun) getXMLDesc(state multistep.StateBag) (string, error) {
	config := state.Get("config").(*Config)
	netName := state.Get("net").(string)
	vncIP := config.VNCBindAddress
	vncPort := state.Get("vnc_port").(int)
	vncPassword := state.Get("vnc_password").(string)

	isoPath := state.Get("iso_path").(string)

	var disks []Disk
	if !config.DiskImage {
		qemu_disk_paths := state.Get("qemu_disk_paths").([]string)
		for i, diskPath := range qemu_disk_paths {
			if fullPath, err := filepath.Abs(diskPath); err != nil {
				return "", err
			} else {
				disks = append(disks, Disk{
					config.Format,
					fullPath,
					diskInterfaceToDev[config.DiskInterface] + fmt.Sprintf("%c", 'a'+i),
					config.DiskCache,
					config.DiskDiscard,
					config.DetectZeroes,
					config.DiskInterface,
				})
			}
		}
	} else {
		vmName := config.VMName
		outputDir, _ := filepath.Abs(config.OutputDir)
		imgPath := filepath.Join(outputDir, vmName)
		disks = append(disks, Disk{
			config.Format,
			imgPath,
			diskInterfaceToDev[config.DiskInterface] + fmt.Sprintf("%c", 'a'+0),
			config.DiskCache,
			config.DiskDiscard,
			config.DetectZeroes,
			config.DiskInterface,
		})
	}

	floppyPath := ""
	if floppyPathRaw, ok := state.GetOk("floppy_path"); ok {
		floppyPath = floppyPathRaw.(string)
	}

	if config.XMLFile != "" {
		s.ui.Say("Overriding defaults libvirt xml with user defined xml")
		oriData, err := ioutil.ReadFile(config.XMLFile)
		if err != nil {
			return "", err
		}

		configCtx := config.ctx
		configCtx.Data = &XMLTemplateData{
			VncIP:       vncIP,
			VncPort:     vncPort,
			VncPassword: vncPassword,
			VMName:      config.VMName,
			Disks:       disks,
			IsoPath:     isoPath,
		}

		userData, err := interpolate.Render(string(oriData), &configCtx)
		if err != nil {
			return "", err
		}
		s.ui.Say(userData)
		return userData, err
	}

	libvirtXML := LibvirtXML{
		Hypervisor:  config.Hypervisor,
		Name:        config.VMName,
		Vcpu:        config.CpuCount,
		Memory:      config.MemorySize,
		Loader:      config.Loader,
		Arch:        config.Arch,
		Machine:     config.MachineType,
		CPUMode:     config.CPUMode,
		Emulator:    config.EmulatorBinary,
		DiskImage:   config.DiskImage,
		Disks:       disks,
		Cdrom:       Cdrom{Source: isoPath},
		FloppyPath:  floppyPath,
		NetName:     netName,
		NetDevice:   config.NetDevice,
		VncIP:       vncIP,
		VncPort:     vncPort,
		VncPassword: vncPassword,
	}
	t, err := template.New("xml").Parse(XmlTemplate)
	if err != nil {
		return "", err
	}
	b := strings.Builder{}
	if err = t.Execute(&b, libvirtXML); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s *stepRun) Cleanup(state multistep.StateBag) {
	driver := state.Get("driver").(Driver)
	ui := state.Get("ui").(packersdk.Ui)

	if err := driver.Stop(); err != nil {
		ui.Error(fmt.Sprintf("Error shutting down VM: %s", err))
	}
}
