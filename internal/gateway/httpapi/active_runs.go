package httpapi

import "selfmind/internal/control"

func (d *Server) beginActive(personID string, run *activeRun) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		d.active = make(map[string]*activeRun)
	}
	if _, exists := d.active[personID]; exists {
		return false
	}
	d.active[personID] = run
	return true
}

func (d *Server) updateActive(personID string, task *control.Task, run *control.Run) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return
	}
	active := d.active[personID]
	if active == nil {
		return
	}
	if task != nil {
		active.TaskID = task.ID
	}
	if run != nil {
		active.RunID = run.ID
		active.StartedAt = run.StartedAt
	}
}

func (d *Server) currentActive(personID string) *activeRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	active := d.active[personID]
	if active == nil {
		return nil
	}
	copy := *active
	return &copy
}

func (d *Server) stopActive(personID string) *activeRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	active := d.active[personID]
	if active == nil {
		return nil
	}
	if active.Cancel != nil {
		active.Cancel()
	}
	copy := *active
	return &copy
}

func (d *Server) endActive(personID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		delete(d.active, personID)
	}
}
