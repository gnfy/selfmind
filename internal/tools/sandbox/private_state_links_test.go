package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// A symlink on the way to private state is pinned like a directory above it.
// These tests pin how the policies render it; the real enforcement is in
// private_state_links_darwin_test.go.

func TestSeatbeltProfileDeniesUnlinkOfPrivateSymlinks(t *testing.T) {
	policy := Policy{
		WritableRoots: []string{"/home/u"},
		Private: PrivateState{
			Dirs:     []string{"/home/u/dotfiles/selfmind/data/gateway"},
			Files:    []string{"/home/u/dotfiles/config.yaml"},
			Symlinks: []string{"/home/u/.selfmind", "/home/u/.selfmind/config.yaml"},
		},
	}
	profile, params, ok := SeatbeltProfile(policy)
	if !ok {
		t.Fatal("the policy must render")
	}
	for _, want := range []string{
		`(deny file-write-unlink (literal (param "PRIVATE_LINK_0")))`,
		`(deny file-write-unlink (literal (param "PRIVATE_LINK_1")))`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("missing %s in:\n%s", want, profile)
		}
	}
	for _, want := range []string{"-DPRIVATE_LINK_0=/home/u/.selfmind", "-DPRIVATE_LINK_1=/home/u/.selfmind/config.yaml"} {
		if !containsParam(params, want) {
			t.Fatalf("missing %s in %v", want, params)
		}
	}
	if strings.Contains(profile, "/home/u/.selfmind") {
		t.Fatalf("link paths must travel as parameters, never in the policy text:\n%s", profile)
	}
	if strings.Index(profile, "PRIVATE_LINK_0") < strings.LastIndex(profile, "(allow ") {
		t.Fatal("link denials must follow every allow rule")
	}
}

func TestOnlyLinksStandingInAWritableDirectoryArePinned(t *testing.T) {
	links := []string{"/tmp", "/home/u/.selfmind", "/srv/shared/link", "/home/u/.selfmind/config.yaml", "/home/u/.selfmind"}
	cases := []struct {
		name   string
		policy Policy
		want   []string
	}{
		{"home is writable", Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{Symlinks: links}},
			[]string{"/home/u/.selfmind", "/home/u/.selfmind/config.yaml"}},
		{"a link in the root itself", Policy{WritableRoots: []string{"/home/u/.selfmind"}, Private: PrivateState{Symlinks: links}},
			[]string{"/home/u/.selfmind/config.yaml"}},
		{"the run scratch counts", Policy{ScratchTmp: "/srv/shared", Private: PrivateState{Symlinks: links}},
			[]string{"/srv/shared/link"}},
		{"a state write path counts", Policy{StateWritePaths: []string{"/srv/shared"}, Private: PrivateState{Symlinks: links}},
			[]string{"/srv/shared/link"}},
		{"a narrow root pins none", Policy{WritableRoots: []string{"/home/u/project"}, Private: PrivateState{Symlinks: links}}, nil},
		{"unsound entries are not pinned here", Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{Symlinks: []string{"relative", "/home/u/../u/x"}}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := protectedSymlinks(tc.policy); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("protectedSymlinks = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSeatbeltProfileRefusesUnsoundSymlinks(t *testing.T) {
	for name, link := range map[string]string{"relative": ".selfmind", "unclean": "/home/u/../u/.selfmind"} {
		t.Run(name, func(t *testing.T) {
			// Dropping the entry would leave a link a command can replace.
			if _, _, ok := SeatbeltProfile(Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{Symlinks: []string{link}}}); ok {
				t.Fatalf("link %q must not render", link)
			}
		})
	}
}

// A link's own directory is as movable as any other: moving it carries the link
// off the name the daemon uses.
func TestTheDirectoryOfAPrivateSymlinkIsPinned(t *testing.T) {
	policy := Policy{
		WritableRoots: []string{"/home/u"},
		Private:       PrivateState{Symlinks: []string{"/home/u/links/deeper/.selfmind"}},
	}
	want := []string{"/home/u/links", "/home/u/links/deeper"}
	if got := protectedAncestors(policy); !reflect.DeepEqual(got, want) {
		t.Fatalf("protectedAncestors = %v, want %v", got, want)
	}
}

// Bubblewrap cannot pin a link: it is a directory entry, not a mount point. Its
// argv does not change, and the resolved path the link leads to is pinned as any
// other.
func TestBubblewrapIgnoresSymlinkEntries(t *testing.T) {
	base := Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{Dirs: []string{"/home/u/dotfiles/data/gateway"}}}
	with := base
	with.Private.Symlinks = []string{"/home/u/.selfmind"}
	if !reflect.DeepEqual(WrapArgv("/usr/bin/bwrap", base, []string{"true"}), WrapArgv("/usr/bin/bwrap", with, []string{"true"})) {
		t.Fatal("a symlink entry must not change the bubblewrap argv")
	}
	if !strings.Contains(strings.Join(WrapArgv("/usr/bin/bwrap", with, []string{"true"}), " "), "--bind-try /home/u/dotfiles/data /home/u/dotfiles/data") {
		t.Fatal("the resolved directory a link leads to is still pinned")
	}
}
