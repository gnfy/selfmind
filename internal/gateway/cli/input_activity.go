package cli

// Presence honesty (client mode): the daemon should treat this terminal as
// "attached" only while the person is actually here, and the only honest
// signal for that is user INPUT. The client shell installs a hook that stamps
// a shared last-input tracker (gateway/client.InputTracker); every keystroke
// entering the Bubble Tea update loop touches it, and the client's presence
// beats/polls derive their active=0|1 claim from the input age. Kept out of
// controller.go so the controller change stays a single call site.

// SetInputActivityHook installs fn to be called on every user keystroke.
// Client mode wires it to the shared input tracker's Touch; nil disables the
// hook (in-process mode has no presence heartbeats to feed).
func (c *Controller) SetInputActivityHook(fn func()) {
	if c == nil || c.model == nil {
		return
	}
	c.model.onUserInput = fn
}
