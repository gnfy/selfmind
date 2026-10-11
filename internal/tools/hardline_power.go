package tools

import "strings"

// Powering the host down and erasing a disk have names beyond shutdown and
// mkfs: systemd's verbs, BSD and macOS newfs, and macOS diskutil.

// systemctlOptions is systemctl's getopt_long grammar, enough to find its
// verb: the first operand.
var systemctlOptions = prefixWrapper{
	valueFlags: wordSet("-t", "--type", "-p", "--property", "-P", "-s", "--signal", "-n", "--lines",
		"-o", "--output", "-H", "--host", "-M", "--machine", "-C", "--capsule", "--job-mode", "--kill-whom",
		"--kill-value", "--what", "--message", "--root", "--image", "--image-policy", "--preset-mode",
		"--boot-loader-menu", "--boot-loader-entry", "--reboot-argument", "--timestamp",
		"--check-inhibitors", "--legend", "--drop-in", "--when", "--state"),
}

// systemctlPowerVerbs power the host down or restart it out from under every
// task, as does starting or isolating their targets.
var systemctlPowerVerbs = wordSet("reboot", "poweroff", "halt", "kexec", "soft-reboot")

// systemctlPowerVerb names the systemctl verb, and target, that powers the
// host down or restarts it.
func systemctlPowerVerb(args []string) (string, bool) {
	rest := systemctlOptions.wrapperOptions(args).rest
	if len(rest) == 0 {
		return "", false
	}
	verb := rest[0]
	if _, power := systemctlPowerVerbs[verb]; power {
		return verb, true
	}
	if verb == "start" || verb == "isolate" || verb == "restart" {
		for _, unit := range rest[1:] {
			if target, ok := strings.CutSuffix(unit, ".target"); ok {
				if _, power := systemctlPowerVerbs[target]; power {
					return verb + " " + unit, true
				}
			}
		}
	}
	return "", false
}

// diskutilEraseVerbs erase or repartition a whole disk or volume; diskutil
// reads its verbs without regard to case.
var diskutilEraseVerbs = wordSet("erasedisk", "erasevolume", "reformat", "zerodisk", "randomdisk",
	"partitiondisk", "secureerase")

// diskutilEraseVerb names the diskutil verb (after an optional `quiet`) that
// erases a disk or volume. secureErase freespace overwrites only the space no
// file uses.
func diskutilEraseVerb(args []string) (string, bool) {
	if len(args) > 0 && strings.EqualFold(args[0], "quiet") {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", false
	}
	verb := strings.ToLower(args[0])
	if _, erases := diskutilEraseVerbs[verb]; !erases {
		return "", false
	}
	if verb == "secureerase" && len(args) > 1 && strings.EqualFold(args[1], "freespace") {
		return "", false
	}
	return args[0], true
}
