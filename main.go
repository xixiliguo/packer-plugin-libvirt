package main

import (
	"fmt"
	"os"
	"packer-plugin-libvirt/builder/libvirt"
	libvirtVersion "packer-plugin-libvirt/version"

	"github.com/hashicorp/packer-plugin-sdk/plugin"
)

func main() {
	pps := plugin.NewSet()
	pps.RegisterBuilder(plugin.DEFAULT_NAME, new(libvirt.Builder))
	pps.SetVersion(libvirtVersion.PluginVersion)
	err := pps.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
