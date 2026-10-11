package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// Private names guard the daemon's state only while they keep naming it, so the
// directories above that state are pinned against rename, swap and removal.
// These tests pin which directories those are and how each backend renders
// them (platform-independent); the real enforcement is exercised under
// sandbox-exec in private_state_moves_darwin_test.go.

func TestProtectedAncestorsAreTheDirectoriesAboveStateInsideWritableRoots(t *testing.T) {
	data := PrivateState{
		Dirs:    []string{"/home/u/.selfmind/gateway", "/home/u/.selfmind/backups"},
		Files:   []string{"/home/u/.selfmind/control.db"},
		Sockets: []string{"/home/u/.selfmind/gateway/control.sock"},
	}
	cases := []struct {
		name   string
		policy Policy
		want   []string
	}{
		{"home is the writable root", Policy{WritableRoots: []string{"/home/u"}, Private: data}, []string{"/home/u/.selfmind"}},
		{
			"a deeper data directory pins every directory down to it, shallowest first",
			Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{Dirs: []string{"/home/u/a/b/.selfmind/gateway"}}},
			[]string{"/home/u/a", "/home/u/a/b", "/home/u/a/b/.selfmind"},
		},
		{
			"a narrower root pins only what is inside it",
			Policy{WritableRoots: []string{"/home/u/a"}, Private: PrivateState{Dirs: []string{"/home/u/a/b/.selfmind/gateway"}}},
			[]string{"/home/u/a/b", "/home/u/a/b/.selfmind"},
		},
		{"a root beside the data directory pins nothing", Policy{WritableRoots: []string{"/home/u/project"}, Private: data}, nil},
		{"a root below the data directory pins nothing", Policy{WritableRoots: []string{"/home/u/.selfmind/workspaces/w"}, Private: data}, nil},
		{
			"a root that is the data directory is pinned as a root already",
			Policy{WritableRoots: []string{"/home/u/.selfmind"}, Private: PrivateState{Files: []string{"/home/u/.selfmind/control.db"}}},
			nil,
		},
		{
			"the run scratch counts as a root",
			Policy{ScratchTmp: "/home/u", Private: data},
			[]string{"/home/u/.selfmind"},
		},
		{
			// Tool state written in place is a writable subpath like a root: a
			// directory between it and private state is as movable.
			"a state write path counts as a root",
			Policy{StateWritePaths: []string{"/home/u/.cache/tool"}, Private: PrivateState{Dirs: []string{"/home/u/.cache/tool/a/b/.selfmind/gateway"}}},
			[]string{"/home/u/.cache/tool/a", "/home/u/.cache/tool/a/b", "/home/u/.cache/tool/a/b/.selfmind"},
		},
		{
			"a state write path beside the data directory pins nothing",
			Policy{StateWritePaths: []string{"/home/u/.aws/sso/cache"}, Private: data},
			nil,
		},
		{
			"a state write path is itself guarded by its own rules, not pinned as an ancestor",
			Policy{StateWritePaths: []string{"/home/u/.cache/tool"}, Private: PrivateState{Dirs: []string{"/home/u/.cache/tool/state"}}},
			nil,
		},
		{
			"a scratch beneath the data directory pins nothing",
			Policy{ScratchTmp: "/home/u/.selfmind/runtime/leases/l/tmp", Private: data},
			nil,
		},
		{
			"a socket outside the data directory pins its own directory",
			Policy{WritableRoots: []string{"/private/tmp/w"}, Private: PrivateState{Sockets: []string{"/private/tmp/w/selfmind-501/h.sock"}}},
			[]string{"/private/tmp/w/selfmind-501"},
		},
		{
			"what lies inside a private directory is hidden with it",
			Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{
				Dirs:  []string{"/home/u/.selfmind/gateway"},
				Files: []string{"/home/u/.selfmind/gateway/state.json"},
			}},
			[]string{"/home/u/.selfmind"},
		},
		{
			"overlapping roots list a directory once",
			Policy{WritableRoots: []string{"/home/u", "/home/u/.selfmind/workspaces/w", "/home"}, Private: data},
			[]string{"/home/u", "/home/u/.selfmind"},
		},
		{"nothing private pins nothing", Policy{WritableRoots: []string{"/home/u"}}, nil},
		{
			"listeners alone pin nothing",
			Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{TCPPorts: []TCPPort{{Port: 8765}}}},
			nil,
		},
		{
			"unsound entries are left to the policy builder, which refuses them",
			Policy{WritableRoots: []string{"/home/u"}, Private: PrivateState{Files: []string{"control.db", "/home/u/../u/x/control.db"}}},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := protectedAncestors(tc.policy); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("protectedAncestors = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSeatbeltProfileDeniesUnlinkOfPrivateAncestorsAndSockets(t *testing.T) {
	policy := Policy{
		WritableRoots: []string{"/home/u"},
		Network:       true,
		Private: PrivateState{
			Dirs:    []string{"/home/u/.selfmind/gateway"},
			Files:   []string{"/home/u/.selfmind/control.db"},
			Sockets: []string{"/home/u/.selfmind/gateway/control.sock"},
		},
	}
	profile, params, ok := SeatbeltProfile(policy)
	if !ok {
		t.Fatal("the policy must render")
	}
	for _, rule := range []string{
		`(deny file-write-unlink (literal (param "PRIVATE_ANCESTOR_0")))`,
		`(deny file-write-unlink (literal (param "PRIVATE_SOCKET_0")))`,
		`(deny network-outbound (literal (param "PRIVATE_SOCKET_0")))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Fatalf("missing %s in:\n%s", rule, profile)
		}
	}
	if !containsParam(params, "-DPRIVATE_ANCESTOR_0=/home/u/.selfmind") {
		t.Fatalf("the data directory must be bound as an ancestor: %v", params)
	}
	// /home/u is a writable root, already denied unlink as a root; /home is not
	// writable at all.
	for _, param := range params {
		if strings.HasPrefix(param, "-DPRIVATE_ANCESTOR_") && !strings.HasSuffix(param, "=/home/u/.selfmind") {
			t.Fatalf("only the data directory lies between the root and the state: %v", params)
		}
	}
	// Seatbelt applies the last matching rule, so the denials must follow every
	// allow, including the writable root that contains the directory.
	if strings.Index(profile, "PRIVATE_ANCESTOR_0") < strings.LastIndex(profile, "(allow ") {
		t.Fatal("ancestor denials must come after every allow rule")
	}

	// The constraint that must change the result: a root that does not contain
	// the data directory leaves nothing to pin, and says nothing about it.
	narrow := policy
	narrow.WritableRoots = []string{"/home/u/project"}
	profile, params, ok = SeatbeltProfile(narrow)
	if !ok || strings.Contains(profile, "PRIVATE_ANCESTOR") {
		t.Fatalf("a narrower root must not render ancestor rules (ok=%v):\n%s", ok, profile)
	}
	for _, param := range params {
		if strings.Contains(param, "ANCESTOR") {
			t.Fatalf("a narrower root must not bind ancestors: %v", params)
		}
	}
}

func containsParam(params []string, want string) bool {
	for _, param := range params {
		if param == want {
			return true
		}
	}
	return false
}

func TestWrapArgvPinsAncestorsOfPrivateStateAsMountPoints(t *testing.T) {
	argv := WrapArgv("/usr/bin/bwrap", Policy{
		WritableRoots: []string{"/home/u"},
		OverlayMounts: []OverlayMount{{Source: "/host/state", Target: "/home/u/.config/tool"}},
		Private: PrivateState{
			Dirs:    []string{"/home/u/.selfmind/gateway"},
			Files:   []string{"/home/u/.selfmind/control.db"},
			Sockets: []string{"/home/u/.selfmind/gateway/control.sock"},
		},
	}, []string{"sh", "-c", "true"})
	joined := " " + strings.Join(argv, " ") + " "
	rootAt := strings.Index(joined, " --bind-try /home/u /home/u ")
	pinAt := strings.Index(joined, " --bind-try /home/u/.selfmind /home/u/.selfmind ")
	overlayAt := strings.Index(joined, " --bind-try /host/state /home/u/.config/tool ")
	maskAt := strings.Index(joined, " --tmpfs /home/u/.selfmind/gateway ")
	if rootAt < 0 || pinAt < 0 || overlayAt < 0 || maskAt < 0 {
		t.Fatalf("missing a bind: %s", joined)
	}
	// The pin is a mount over a directory the root already exposes, so it must
	// come after that root; the overlay and the masks land inside it.
	if !(rootAt < pinAt && pinAt < overlayAt && pinAt < maskAt) {
		t.Fatalf("the pin must follow the writable root and precede overlays and masks: %s", joined)
	}
	if strings.Count(joined, " --bind-try /home/u/.selfmind ") != 1 {
		t.Fatalf("each directory is pinned once: %s", joined)
	}
	// The root is already a mount point and /home is not writable: neither is
	// bound again, which would widen /home.
	if strings.Contains(joined, " --bind-try /home /home ") || strings.Count(joined, " --bind-try /home/u /home/u ") != 1 {
		t.Fatalf("only directories between the root and the state are pinned: %s", joined)
	}
}

func TestWrapArgvDoesNotPinWhatTheSandboxCannotSeeOrDoesNotOwn(t *testing.T) {
	t.Run("nothing under /tmp", func(t *testing.T) {
		// /tmp is the run scratch or a private tmpfs: a directory named there
		// would be created in it on the host.
		argv := WrapArgv("/usr/bin/bwrap", Policy{
			WritableRoots: []string{"/tmp/w"},
			Private:       PrivateState{Dirs: []string{"/tmp/w/x/gateway"}},
		}, []string{"true"})
		if strings.Contains(strings.Join(argv, " "), "/tmp/w/x") {
			t.Fatalf("an ancestor under /tmp must not be bound: %v", argv)
		}
	})
	t.Run("nothing inside a synthesized or overlaid directory", func(t *testing.T) {
		// Those paths are the sandbox's own tmpfs or redirected state; binding the
		// host directory there would put the host's content back.
		argv := WrapArgv("/usr/bin/bwrap", Policy{
			WritableRoots:   []string{"/home/u"},
			SynthesizedDirs: []SynthesizedDir{{Target: "/home/u/.aws"}},
			OverlayMounts:   []OverlayMount{{Source: "/host/cache", Target: "/home/u/.cache/tool"}},
			Private: PrivateState{Dirs: []string{
				"/home/u/.aws/x/private", "/home/u/.cache/tool/y/private",
			}},
		}, []string{"true"})
		joined := strings.Join(argv, " ")
		for _, forbidden := range []string{"--bind-try /home/u/.aws/x ", "--bind-try /home/u/.cache/tool/y "} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("%q must not be bound: %s", forbidden, joined)
			}
		}
	})
	t.Run("nothing when no root contains the state", func(t *testing.T) {
		base := Policy{WritableRoots: []string{"/home/u/project"}}
		with := base
		with.Private = PrivateState{Dirs: []string{"/home/u/.selfmind/gateway"}}
		bare := strings.Join(WrapArgv("/usr/bin/bwrap", base, []string{"true"}), " ")
		got := strings.Join(WrapArgv("/usr/bin/bwrap", with, []string{"true"}), " ")
		if strings.Contains(got, "--bind-try /home/u/.selfmind") {
			t.Fatalf("a root beside the data directory must pin nothing: %s", got)
		}
		if !strings.Contains(got, "--tmpfs /home/u/.selfmind/gateway") || strings.Contains(bare, "/home/u/.selfmind") {
			t.Fatalf("the mask is unchanged by pinning: %s", got)
		}
	})
}
