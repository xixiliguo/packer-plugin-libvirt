package libvirt

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

const BuilderId = "xixiliguo.libvirt"

type Builder struct {
	config Config
	runner multistep.Runner
}

func (b *Builder) ConfigSpec() hcldec.ObjectSpec { return b.config.FlatMapstructure().HCL2Spec() }

func (b *Builder) Prepare(raws ...interface{}) ([]string, []string, error) {
	warnings, errs := b.config.Prepare(raws...)
	if errs != nil {
		return nil, warnings, errs
	}

	return nil, warnings, nil
}

func (b *Builder) Run(ctx context.Context, ui packersdk.Ui, hook packersdk.Hook) (packersdk.Artifact, error) {
	// Create the driver that we'll use to communicate with Libvirt
	driver, netName, err := b.newDriver(b.config.LibvirtAddr, b.config.NetBridge)
	if err != nil {
		return nil, fmt.Errorf("Failed creating Libvirt driver: %s", err)
	}

	steps := []multistep.Step{}
	if !b.config.ISOSkipCache {
		steps = append(steps, &commonsteps.StepDownload{
			Checksum:    b.config.ISOChecksum,
			Description: "ISO",
			Extension:   b.config.TargetExtension,
			ResultKey:   "iso_path",
			TargetPath:  b.config.TargetPath,
			Url:         b.config.ISOUrls,
		})
	} else {
		steps = append(steps, &stepSetISO{
			ResultKey: "iso_path",
			Url:       b.config.ISOUrls,
		})
	}

	steps = append(steps, new(stepPrepareOutputDir),
		&commonsteps.StepCreateFloppy{
			Files:       b.config.FloppyConfig.FloppyFiles,
			Directories: b.config.FloppyConfig.FloppyDirectories,
			Label:       b.config.FloppyConfig.FloppyLabel,
		},
		&commonsteps.StepCreateCD{
			Files: b.config.CDConfig.CDFiles,
			Label: b.config.CDConfig.CDLabel,
		},
		&stepCreateDisk{
			AdditionalDiskSize: b.config.AdditionalDiskSize,
			DiskImage:          b.config.DiskImage,
			DiskSize:           b.config.DiskSize,
			Format:             b.config.Format,
			OutputDir:          b.config.OutputDir,
			UseBackingFile:     b.config.UseBackingFile,
			VMName:             b.config.VMName,
			QemuImgArgs:        b.config.QemuImgArgs,
		},
		&stepCopyDisk{
			DiskImage:      b.config.DiskImage,
			Format:         b.config.Format,
			OutputDir:      b.config.OutputDir,
			UseBackingFile: b.config.UseBackingFile,
			VMName:         b.config.VMName,
		},
		&stepResizeDisk{
			DiskCompression: b.config.DiskCompression,
			DiskImage:       b.config.DiskImage,
			Format:          b.config.Format,
			OutputDir:       b.config.OutputDir,
			SkipResizeDisk:  b.config.SkipResizeDisk,
			VMName:          b.config.VMName,
			DiskSize:        b.config.DiskSize,
			QemuImgArgs:     b.config.QemuImgArgs,
		},
		new(stepHTTPIPDiscover),
		&commonsteps.StepHTTPServer{
			HTTPDir:     b.config.HTTPDir,
			HTTPPortMin: b.config.HTTPPortMin,
			HTTPPortMax: b.config.HTTPPortMax,
			HTTPAddress: b.config.HTTPAddress,
		},
		new(stepConfigureVNC),
		&stepRun{
			DiskImage: b.config.DiskImage,
		},
		&stepTypeBootCommand{},
		&communicator.StepConnect{
			Config:    &b.config.Comm,
			Host:      commHost(b.config.Comm.Host()),
			SSHConfig: b.config.Comm.SSHConfigFunc(),
			SSHPort:   commPort,
			WinRMPort: commPort,
		},
		new(commonsteps.StepProvision),
		&commonsteps.StepCleanupTempKeys{
			Comm: &b.config.Comm,
		},
		&stepShutdown{
			ShutdownTimeout: b.config.ShutdownTimeout,
			ShutdownCommand: b.config.ShutdownCommand,
			Comm:            &b.config.Comm,
		},
		&stepConvertDisk{
			DiskCompression: b.config.DiskCompression,
			Format:          b.config.Format,
			OutputDir:       b.config.OutputDir,
			SkipCompaction:  b.config.SkipCompaction,
			VMName:          b.config.VMName,
			QemuImgArgs:     b.config.QemuImgArgs,
		},
	)

	// Setup the state bag
	state := new(multistep.BasicStateBag)
	state.Put("config", &b.config)
	state.Put("debug", b.config.PackerDebug)
	state.Put("driver", driver)
	state.Put("hook", hook)
	state.Put("ui", ui)
	state.Put("net", netName)
	// Run
	b.runner = commonsteps.NewRunnerWithPauseFn(steps, b.config.PackerConfig, ui, state)
	b.runner.Run(ctx, state)

	// If there was an error, return that
	if rawErr, ok := state.GetOk("error"); ok {
		return nil, rawErr.(error)
	}

	// If we were interrupted or cancelled, then just exit.
	if _, ok := state.GetOk(multistep.StateCancelled); ok {
		return nil, errors.New("Build was cancelled.")
	}

	if _, ok := state.GetOk(multistep.StateHalted); ok {
		return nil, errors.New("Build was halted.")
	}

	// Compile the artifact list
	files := make([]string, 0, 5)
	visit := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}

		return nil
	}

	if err := filepath.Walk(b.config.OutputDir, visit); err != nil {
		return nil, err
	}

	artifact := &Artifact{
		dir:   b.config.OutputDir,
		f:     files,
		state: make(map[string]interface{}),
	}

	artifact.state["generated_data"] = state.Get("generated_data")
	artifact.state["diskName"] = b.config.VMName

	// placed in state in step_create_disk.go
	diskpaths, ok := state.Get("qemu_disk_paths").([]string)
	if ok {
		artifact.state["diskPaths"] = diskpaths
	}
	artifact.state["diskType"] = b.config.Format
	artifact.state["diskSize"] = b.config.DiskSize
	artifact.state["hypervisor"] = b.config.Hypervisor

	return artifact, nil
}

func (b *Builder) newDriver(address string, netBridge string) (Driver, string, error) {
	network := "unix"
	if _, _, err := net.SplitHostPort(address); err == nil {
		network = "tcp"
	}
	conn, err := net.DialTimeout(network, address, 2*time.Second)
	if err != nil {
		return nil, "", fmt.Errorf("%s %s: %v", network, address, err)
	}
	l := libvirt.New(conn)
	if err := l.Connect(); err != nil {
		return nil, "", fmt.Errorf("%s %s: %v", network, address, err)

	}

	qemuImgPath, err := exec.LookPath("qemu-img")
	if err != nil {
		return nil, "", err
	}

	log.Printf("Libvirt connection info: %s %s, Qemu Image Path: %s", network, address, qemuImgPath)
	driver := &LibvirtDriver{
		libvirt:     l,
		QemuImgPath: qemuImgPath,
		netBridge:   netBridge,
	}
	if err := driver.Verify(); err != nil {
		return nil, "", err
	}
	return driver, driver.vmNet.Name, nil
}
