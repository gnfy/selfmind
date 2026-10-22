package tools

// homebrewGNUPrograms are the programs of the GNU packages Homebrew installs
// beside the BSD tools macOS ships with a g prefix — coreutils, findutils,
// time, inetutils, sed, tar, grep, make, awk, indent, which, libtool, units
// and ed — so grm is rm, gtimeout is timeout, gsed is sed and gmake is make.
// programName reads such a name as the program it is, so the bare program's
// rules apply (sed and make back no class); proof and remembered classes
// still need the exact word, which a prefixed name never is.
var homebrewGNUPrograms = func() map[string]struct{} {
	programs := map[string]struct{}{}
	for _, set := range [][]string{gnuCoreutils, gnuFindutils, {"time"}, gnuInetutils, gnuTextutils} {
		for _, program := range set {
			programs[program] = struct{}{}
		}
	}
	return programs
}()

var (
	gnuCoreutils = []string{
		"[", "arch", "b2sum", "base32", "base64", "basename", "basenc", "cat", "chcon", "chgrp", "chmod",
		"chown", "chroot", "cksum", "comm", "coreutils", "cp", "csplit", "cut", "date", "dd", "df", "dir",
		"dircolors", "dirname", "du", "echo", "env", "expand", "expr", "factor", "false", "fmt", "fold",
		"groups", "head", "hostid", "hostname", "id", "install", "join", "kill", "link", "ln", "logname",
		"ls", "md5sum", "mkdir", "mkfifo", "mknod", "mktemp", "mv", "nice", "nl", "nohup", "nproc",
		"numfmt", "od", "paste", "pathchk", "pinky", "pr", "printenv", "printf", "ptx", "pwd", "readlink",
		"realpath", "rm", "rmdir", "runcon", "seq", "sha1sum", "sha224sum", "sha256sum", "sha384sum",
		"sha512sum", "shred", "shuf", "sleep", "sort", "split", "stat", "stdbuf", "stty", "sum", "sync",
		"tac", "tail", "tee", "test", "timeout", "touch", "tr", "true", "truncate", "tsort", "tty",
		"uname", "unexpand", "uniq", "unlink", "uptime", "users", "vdir", "wc", "who", "whoami", "yes",
	}
	gnuFindutils = []string{"find", "xargs", "locate", "updatedb"}
	gnuInetutils = []string{
		"dnsdomainname", "ftp", "hostname", "ifconfig", "logger", "ping", "ping6", "rcp", "rexec", "rlogin",
		"rsh", "talk", "telnet", "tftp", "traceroute", "whois",
	}
	// gnu-sed, gnu-tar, grep, make, gawk, gnu-indent, gnu-which, libtool,
	// gnu-units and ed.
	gnuTextutils = []string{
		"sed", "tar", "grep", "egrep", "fgrep", "make", "awk", "indent", "which", "libtool", "libtoolize",
		"units", "ed", "red",
	}
)
