//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type Config,QemuImgArgs

package libvirt

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/shutdowncommand"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

var hypervisors = map[string]struct{}{
	"kvm":  {},
	"qemu": {},
	"xen":  {},
}

var diskInterface = map[string]bool{
	"ide":         true,
	"scsi":        true,
	"virtio":      true,
	"virtio-scsi": true,
}

var diskInterfaceToDev = map[string]string{
	"ide":         "hd",
	"scsi":        "sd",
	"virtio":      "vd",
	"virtio-scsi": "sd",
}

var diskCache = map[string]bool{
	"writethrough": true,
	"writeback":    true,
	"none":         true,
	"unsafe":       true,
	"directsync":   true,
}

var diskDiscard = map[string]bool{
	"unmap":  true,
	"ignore": true,
}

var diskDZeroes = map[string]bool{
	"unmap": true,
	"on":    true,
	"off":   true,
}

type QemuImgArgs struct {
	Convert []string `mapstructure:"convert" required:"false"`
	Create  []string `mapstructure:"create" required:"false"`
	Resize  []string `mapstructure:"resize" required:"false"`
}

type Config struct {
	common.PackerConfig            `mapstructure:",squash"`
	commonsteps.HTTPConfig         `mapstructure:",squash"`
	commonsteps.ISOConfig          `mapstructure:",squash"`
	bootcommand.VNCConfig          `mapstructure:",squash"`
	shutdowncommand.ShutdownConfig `mapstructure:",squash"`
	Comm                           communicator.Config `mapstructure:",squash"`
	commonsteps.FloppyConfig       `mapstructure:",squash"`
	commonsteps.CDConfig           `mapstructure:",squash"`
	// Use iso from provided url. Qemu must support
	// curl block device. This defaults to `false`.
	ISOSkipCache bool `mapstructure:"iso_skip_cache" required:"false"`
	// The hypervisor type to use when running the VM.
	// This may be `kvm`, `qemu`, `xen`. The default value is kvm
	Hypervisor string `mapstructure:"hypervisor" required:"false"`
	// Additional disks to create. Uses `vm_name` as the disk name template and
	// appends `-#` where `#` is the position in the array. `#` starts at 1 since 0
	// is the default disk. Each string represents the disk image size in bytes.
	// Optional suffixes 'k' or 'K' (kilobyte, 1024), 'M' (megabyte, 1024k), 'G'
	// (gigabyte, 1024M), 'T' (terabyte, 1024G), 'P' (petabyte, 1024T) and 'E'
	// (exabyte, 1024P)  are supported. 'b' is ignored. Per qemu-img documentation.
	// Each additional disk uses the same disk parameters as the default disk.
	// Unset by default.
	AdditionalDiskSize []string `mapstructure:"disk_additional_size" required:"false"`
	// The number of cpus to use when building the VM.
	//  The default is `1` CPU.
	CpuCount int `mapstructure:"cpus" required:"false"`
	// The interface to use for the disk. Allowed values include any of `ide`,
	// `scsi`, `virtio` or `virtio-scsi`^\*. Note also that any boot commands
	// or kickstart type scripts must have proper adjustments for resulting
	// device names. The Qemu builder uses `virtio` by default.
	//
	// ^\* Please be aware that use of the `scsi` disk interface has been
	// disabled by Red Hat due to a bug described
	// [here](https://bugzilla.redhat.com/show_bug.cgi?id=1019220). If you are
	// running Qemu on RHEL or a RHEL variant such as CentOS, you *must* choose
	// one of the other listed interfaces. Using the `scsi` interface under
	// these circumstances will cause the build to fail.
	DiskInterface string `mapstructure:"disk_interface" required:"false"`
	// The size in bytes of the hard disk of the VM. Suffix with the first
	// letter of common byte types. Use "k" or "K" for kilobytes, "M" for
	// megabytes, G for gigabytes, and T for terabytes. If no value is provided
	// for disk_size, Packer uses a default of `40960M` (40 GB). If a disk_size
	// number is provided with no units, Packer will default to Megabytes.
	DiskSize string `mapstructure:"disk_size" required:"false"`
	// Packer resizes the QCOW2 image using
	// qemu-img resize.  Set this option to true to disable resizing.
	// Defaults to false.
	SkipResizeDisk bool `mapstructure:"skip_resize_disk" required:"false"`
	// The cache mode to use for disk. Allowed values include any of
	// `writethrough`, `writeback`, `none`, `unsafe` or `directsync`. By
	// default, this is set to `writeback`.
	DiskCache string `mapstructure:"disk_cache" required:"false"`
	// The discard mode to use for disk. Allowed values
	// include any of unmap or ignore. By default, this is set to ignore.
	DiskDiscard string `mapstructure:"disk_discard" required:"false"`
	// The detect-zeroes mode to use for disk.
	// Allowed values include any of unmap, on or off. Defaults to off.
	// When the value is "off" we don't set the flag in the qemu command, so that
	// Packer still works with old versions of QEMU that don't have this option.
	DetectZeroes string `mapstructure:"disk_detect_zeroes" required:"false"`
	// Packer compacts the QCOW2 image using
	// qemu-img convert.  Set this option to true to disable compacting.
	// Defaults to false.
	SkipCompaction bool `mapstructure:"skip_compaction" required:"false"`
	// Apply compression to the QCOW2 disk file
	// using qemu-img convert. Defaults to false.
	DiskCompression bool `mapstructure:"disk_compression" required:"false"`
	// Either `qcow2` or `raw`, this specifies the output format of the virtual
	// machine image. This defaults to `qcow2`.
	Format string `mapstructure:"format" required:"false"`
	// Packer defaults to building from an ISO file, this parameter controls
	// whether the ISO URL supplied is actually a bootable QEMU image. When
	// this value is set to `true`, the machine will either clone the source or
	// use it as a backing file (if `use_backing_file` is `true`); then, it
	// will resize the image according to `disk_size` and boot it.
	DiskImage bool `mapstructure:"disk_image" required:"false"`
	// A map of custom arguments to pass to qemu-img commands, where the key
	// is the subcommand, and the values are lists of strings for each flag.
	// Example:
	//
	// In JSON:
	// ```json
	// {
	//  "qemu_img_args": {
	//    "convert": ["-o", "preallocation=full"],
	//	  "resize": ["-foo", "bar"]
	//  }
	// ```
	// Please note
	// that unlike qemuargs, these commands are not split into switch-value
	// sub-arrays, because the basic elements in qemu-img calls are  unlikely
	// to need an actual override.
	// The arguments will be constructed as follows:
	// - Convert:
	// 	Default is `qemu-img convert -O $format $sourcepath $targetpath`. Adding
	// 	arguments ["-foo", "bar"] to qemu_img_args.convert will change this to
	// 	`qemu-img convert -foo bar -O $format $sourcepath $targetpath`
	// - Create:
	// 	Default is `create -f $format $targetpath $size`. Adding arguments
	// 	["-foo", "bar"] to qemu_img_args.create will change this to
	// 	"create -f qcow2 -foo bar target.qcow2 1234M"
	// - Resize:
	// 	Default is `qemu-img resize -f $format $sourcepath $size`. Adding
	// 	arguments ["-foo", "bar"] to qemu_img_args.resize will change this to
	// 	`qemu-img resize -f $format -foo bar $sourcepath $size`
	QemuImgArgs QemuImgArgs `mapstructure:"qemu_img_args" required:"false"`
	// Only applicable when disk_image is true
	// and format is qcow2, set this option to true to create a new QCOW2
	// file that uses the file located at iso_url as a backing file. The new file
	// will only contain blocks that have changed compared to the backing file, so
	// enabling this option can significantly reduce disk usage. If true, Packer
	// will force the `skip_compaction` also to be true as well to skip disk
	// conversion which would render the backing file feature useless.
	UseBackingFile bool `mapstructure:"use_backing_file" required:"false"`
	// The communacation address of libvirt. By default, this is
	// /var/run/libvirt/libvirt-sock
	LibvirtAddr string `mapstructure:"libvirt_addr" required:"false"`
	// The OS arch of emulation to use. Run `virsh capabilities` to
	// list available types for your system. This defaults to `x86_64`.
	Arch string `mapstructure:"arch" required:"false"`
	// The type of machine emulation to use. Run `virsh capabilities` to
	// list available types for  your system. This defaults to `pc`.
	MachineType string `mapstructure:"machine_type" required:"false"`
	// The firmware which is specified by absolute path.
	// It is useful when VM boot on UEFI Mode
	Loader string `mapstructure:"loader" required:"false"`
	// The CPU Mode to configure a guest CPU to be as close to host CPU as possible
	// Allowed values `host-passthrough`, `host-model` and other value
	// `host-passthrough` generate the following xml
	// <cpu mode='host-passthrough'>
	// </cpu>
	// `cortex-a57` generate the following xml
	// <cpu mode='custom'>
	// 	<model>cortex-a57</mode>
	// </cpu>
	CPUMode string `mapstructure:"cpu_mode" equired:"false"`
	// The binary of emulator to use. Run `virsh capabilities` to
	// list available value for your system. This defaults to `/usr/libexec/qemu-kvm`.
	EmulatorBinary string `mapstructure:"emulator_binary" required:"false"`
	// The amount of memory to use when building the VM
	// in megabytes. This defaults to 512 megabytes.
	MemorySize int `mapstructure:"memory" required:"false"`
	// The driver to use for the network interface. Allowed values `ne2k_pci`,
	// `i82551`, `i82557b`, `i82559er`, `rtl8139`, `e1000`, `pcnet`, `virtio`,
	// `virtio-net`, `virtio-net-pci`, `usb-net`, `i82559a`, `i82559b`,
	// `i82559c`, `i82550`, `i82562`, `i82557a`, `i82557c`, `i82801`,
	// `vmxnet3`, `i82558a` or `i82558b`. The Qemu builder uses `virtio-net` by
	// default.
	NetDevice string `mapstructure:"net_device" required:"false"`
	// Connects the network to this bridge
	//
	// **NB** This bridge must already exist. libvirt use `virbr0` bridge
	// as default
	//
	// **NB** This only works in Linux based OSes.
	NetBridge string `mapstructure:"net_bridge" required:"false"`
	// This is the path to the directory where the
	// resulting virtual machine will be created. This may be relative or absolute.
	// If relative, the path is relative to the working directory when packer
	// is executed. This directory must not exist or be empty prior to running
	// the builder. By default this is output-BUILDNAME where "BUILDNAME" is the
	// name of the build.
	OutputDir string `mapstructure:"output_directory" required:"false"`
	// Allow to control libvirt by customized xml
	// This is a template engine and allows access to the following
	// variables: {{ .HTTPIP }}, {{ .HTTPPort }}, {{ .HTTPDir }}, {{ .OutputDir }},
	// and {{ .Name }}
	XMLFile string `mapstructure:"xml_file" required:"false"`
	// The IP address that should be
	// binded to for VNC. By default packer will use 127.0.0.1 for this. If you
	// wish to bind to all interfaces use 0.0.0.0.
	VNCBindAddress string `mapstructure:"vnc_bind_address" required:"false"`
	// Whether or not to set a password on the VNC server. This option
	// automatically enables the QMP socket. See `qmp_socket_path`. Defaults to
	// `false`.
	VNCUsePassword bool `mapstructure:"vnc_use_password" required:"false"`
	// The minimum and maximum port
	// to use for VNC access to the virtual machine. The builder uses VNC to type
	// the initial boot_command. Because Packer generally runs in parallel,
	// Packer uses a randomly chosen port in this range that appears available. By
	// default this is 5900 to 6000. The minimum and maximum ports are inclusive.
	VNCPortMin int `mapstructure:"vnc_port_min" required:"false"`
	VNCPortMax int `mapstructure:"vnc_port_max"`
	// This is the name of the image (QCOW2 or IMG) file for
	// the new virtual machine. By default this is packer-BUILDNAME, where
	// "BUILDNAME" is the name of the build. Currently, no file extension will be
	// used unless it is specified in this option.
	VMName string `mapstructure:"vm_name" required:"false"`
	// The interface to use for the CDROM device which contains the ISO image.
	// Allowed values include any of `ide`, `scsi`, `virtio` or
	// `virtio-scsi`. The Qemu builder uses `virtio` by default.
	// Some ARM64 images require `virtio-scsi`.
	CDROMInterface string `mapstructure:"cdrom_interface" required:"false"`

	ctx interpolate.Context
}

func (c *Config) Prepare(raws ...interface{}) ([]string, error) {
	err := config.Decode(c, &config.DecodeOpts{
		PluginType:         BuilderId,
		Interpolate:        true,
		InterpolateContext: &c.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"boot_command",
			},
		},
	}, raws...)
	if err != nil {
		return nil, err
	}

	// Accumulate any errors and warnings
	var errs *packersdk.MultiError
	warnings := make([]string, 0)

	errs = packersdk.MultiErrorAppend(errs, c.ShutdownConfig.Prepare(&c.ctx)...)

	if c.DiskSize == "" || c.DiskSize == "0" {
		c.DiskSize = "40960M"
	} else {
		// Make sure supplied disk size is valid
		// (digits, plus an optional valid unit character). e.g. 5000, 40G, 1t
		re := regexp.MustCompile(`^[\d]+(b|k|m|g|t){0,1}$`)
		matched := re.MatchString(strings.ToLower(c.DiskSize))
		if !matched {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Invalid disk size."))
		} else {
			// Okay, it's valid -- if it doesn't alreay have a suffix, then
			// append "M" as the default unit.
			re = regexp.MustCompile(`^[\d]+$`)
			matched = re.MatchString(strings.ToLower(c.DiskSize))
			if matched {
				// Needs M added.
				c.DiskSize = fmt.Sprintf("%sM", c.DiskSize)
			}
		}
	}

	if c.DiskCache == "" {
		c.DiskCache = "writeback"
	}

	if c.DiskDiscard == "" {
		c.DiskDiscard = "ignore"
	}

	if c.DetectZeroes == "" {
		c.DetectZeroes = "off"
	}

	if c.Hypervisor == "" {
		// /dev/kvm is a kernel module that may be loaded if kvm is
		// installed and the host supports VT-x extensions. To make sure
		// this will actually work we need to os.Open() it. If os.Open fails
		// the kernel module was not installed or loaded correctly.
		if fp, err := os.Open("/dev/kvm"); err != nil {
			c.Hypervisor = "qemu"
		} else {
			fp.Close()
			c.Hypervisor = "kvm"
		}

		log.Printf("use detected accelerator: %s", c.Hypervisor)
	} else {
		log.Printf("use specified accelerator: %s", c.Hypervisor)
	}

	if c.Arch == "" {
		c.Arch = "x86_64"
	}

	if c.MachineType == "" {
		c.MachineType = "pc"
	}

	if c.OutputDir == "" {
		c.OutputDir = fmt.Sprintf("output-%s", c.PackerBuildName)
	}

	if c.CPUMode == "" {
		c.CPUMode = "host-passthrough"
	}

	if c.EmulatorBinary == "" {
		c.EmulatorBinary = "/usr/libexec/qemu-kvm"
	}
	if emulatorPath, err := exec.LookPath(c.EmulatorBinary); err != nil {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("EmulatorBinary %s is not executeable file", c.EmulatorBinary))
	} else {
		c.EmulatorBinary = emulatorPath
	}

	if c.LibvirtAddr == "" {
		c.LibvirtAddr = "/var/run/libvirt/libvirt-sock"
	}

	if c.MemorySize < 10 {
		log.Printf("MemorySize %d is too small, using default: 512", c.MemorySize)
		c.MemorySize = 512
	}

	if c.CpuCount < 1 {
		log.Printf("CpuCount %d too small, using default: 1", c.CpuCount)
		c.CpuCount = 1
	}

	if c.VNCBindAddress == "" {
		c.VNCBindAddress = "127.0.0.1"
	}

	if c.VNCPortMin == 0 {
		c.VNCPortMin = 5900
	}

	if c.VNCPortMax == 0 {
		c.VNCPortMax = 6000
	}

	if c.VMName == "" {
		c.VMName = fmt.Sprintf("packer-%s", c.PackerBuildName)
	}

	if c.Format == "" {
		c.Format = "qcow2"
	}

	errs = packersdk.MultiErrorAppend(errs, c.FloppyConfig.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.CDConfig.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.VNCConfig.Prepare(&c.ctx)...)

	if c.NetDevice == "" {
		c.NetDevice = "virtio-net"
	}

	if c.DiskInterface == "" {
		c.DiskInterface = "virtio"
	}

	if c.ISOSkipCache {
		c.ISOChecksum = "none"
	}
	isoWarnings, isoErrs := c.ISOConfig.Prepare(&c.ctx)
	warnings = append(warnings, isoWarnings...)
	errs = packersdk.MultiErrorAppend(errs, isoErrs...)

	errs = packersdk.MultiErrorAppend(errs, c.HTTPConfig.Prepare(&c.ctx)...)

	if es := c.Comm.Prepare(&c.ctx); len(es) > 0 {
		errs = packer.MultiErrorAppend(errs, es...)
	}

	if !(c.Format == "qcow2" || c.Format == "raw") {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("invalid format, only 'qcow2' or 'raw' are allowed"))
	}

	if c.Format != "qcow2" {
		c.SkipCompaction = true
		c.DiskCompression = false
	}

	if c.UseBackingFile {
		c.SkipCompaction = true
		if !(c.DiskImage && c.Format == "qcow2") {
			errs = packersdk.MultiErrorAppend(
				errs, errors.New("use_backing_file can only be enabled for QCOW2 images and when disk_image is true"))
		}
	}

	if c.SkipResizeDisk && !(c.DiskImage) {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("skip_resize_disk can only be used when disk_image is true"))
	}

	if _, ok := hypervisors[c.Hypervisor]; !ok {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("invalid hypervisor, only 'kvm', 'qemu', 'xen' are allowed"))
	}

	if _, ok := diskInterface[c.DiskInterface]; !ok {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("unrecognized disk interface type"))
	}

	if _, ok := diskCache[c.DiskCache]; !ok {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("unrecognized disk cache type"))
	}

	if _, ok := diskDiscard[c.DiskDiscard]; !ok {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("unrecognized disk discard type"))
	}

	if _, ok := diskDZeroes[c.DetectZeroes]; !ok {
		errs = packersdk.MultiErrorAppend(
			errs, errors.New("unrecognized disk detect zeroes setting"))
	}

	if !c.PackerForce {
		if _, err := os.Stat(c.OutputDir); err == nil {
			errs = packersdk.MultiErrorAppend(
				errs,
				fmt.Errorf("Output directory '%s' already exists. It must not exist.", c.OutputDir))
		}
	}

	if c.VNCPortMin > c.VNCPortMax {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("vnc_port_min must be less than vnc_port_max"))
	}

	if c.NetBridge == "" {
		c.NetBridge = "virbr0"
	}

	if c.XMLFile != "" {
		if _, err := os.Stat(c.XMLFile); err != nil {
			errs = packersdk.MultiErrorAppend(
				errs,
				fmt.Errorf("User defined XML file '%s' is not exist", c.XMLFile))
		}
	}

	if errs != nil && len(errs.Errors) > 0 {
		return warnings, errs
	}

	return warnings, nil

}
