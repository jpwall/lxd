package sys

import (
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
	"github.com/syndtr/gocapability/capability"
)

// Initialize AppArmor-specific attributes.
func (s *OS) initAppArmor() {
	/* Detect AppArmor availability */
	_, err := exec.LookPath("apparmor_parser")
	if os.Getenv("LXD_SECURITY_APPARMOR") == "false" {
		logger.Warnf("AppArmor support has been manually disabled")
	} else if !shared.IsDir("/sys/kernel/security/apparmor") {
		logger.Warnf("AppArmor support has been disabled because of lack of kernel support")
	} else if err != nil {
		logger.Warnf("AppArmor support has been disabled because 'apparmor_parser' couldn't be found")
	} else {
		s.AppArmorAvailable = true
	}

	/* Detect AppArmor stacking support */
	s.AppArmorStacking = util.AppArmorCanStack()

	/* Detect existing AppArmor stack */
	if shared.PathExists("/sys/kernel/security/apparmor/.ns_stacked") {
		contentBytes, err := ioutil.ReadFile("/sys/kernel/security/apparmor/.ns_stacked")
		if err == nil && string(contentBytes) == "yes\n" {
			s.AppArmorStacked = true
		}
	}

	/* Detect AppArmor admin support */
	if !haveMacAdmin() {
		if s.AppArmorAvailable {
			logger.Warnf("Per-container AppArmor profiles are disabled because the mac_admin capability is missing.")
		}
	} else if s.RunningInUserNS && !s.AppArmorStacked {
		if s.AppArmorAvailable {
			logger.Warnf("Per-container AppArmor profiles are disabled because LXD is running in an unprivileged container without stacking.")
		}
	} else {
		s.AppArmorAdmin = true
	}

	/* Detect AppArmor confinment */
	profile := util.AppArmorProfile()
	if profile != "unconfined" && profile != "" {
		if s.AppArmorAvailable {
			logger.Warnf("Per-container AppArmor profiles are disabled because LXD is already protected by AppArmor.")
		}
		s.AppArmorConfined = true
	}
}

func haveMacAdmin() bool {
	c, err := capability.NewPid(0)
	if err != nil {
		return false
	}
	if c.Get(capability.EFFECTIVE, capability.CAP_MAC_ADMIN) {
		return true
	}
	return false
}