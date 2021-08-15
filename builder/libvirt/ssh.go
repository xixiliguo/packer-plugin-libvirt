package libvirt

import (
	"log"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

func commHost(host string) func(multistep.StateBag) (string, error) {
	return func(state multistep.StateBag) (string, error) {
		if host != "" {
			log.Printf("Using host value: %s", host)
			return host, nil
		}

		driver := state.Get("driver").(*LibvirtDriver)

		return driver.GetDomainIP()
	}
}

func commPort(state multistep.StateBag) (int, error) {
	commHostPort, ok := state.Get("commHostPort").(int)
	if !ok {
		commHostPort = 22
	}
	return commHostPort, nil
}
