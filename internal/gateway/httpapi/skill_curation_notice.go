package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/log"
)

// skillCurationNoticeMaxNames bounds one notice. A curation pass reports what
// changed, not a catalog listing.
const skillCurationNoticeMaxNames = 3

// notifySkillCurationOutcome tells the person what a curation pass did.
//
// The worker used to hash the curator's summary into a job digest and drop it,
// and nothing else said a Skill had been published or that an eligible
// candidate was withheld and why. The only candidate this deployment ever
// produced sat unpromoted for a day and was found by reading the database. A
// learning loop whose terminal states are invisible cannot be observed, so it
// also cannot be corrected — every later change to it would be invisible too.
//
// The notice derives from the durable events the curator wrote, never from its
// summary prose, so the wording of a background job cannot become a
// user-facing contract.
func (d *Server) notifySkillCurationOutcome(ctx context.Context, tenantID, runID string) {
	if d == nil || d.Control == nil {
		return
	}
	notice, run := d.skillCurationNoticeForRun(ctx, tenantID, runID)
	if notice == "" || run == nil {
		return
	}
	identity := d.routeIdentityForPerson(ctx, tenantID, run.PersonID, run.Channel, "cli", nil)
	if identity == nil {
		return
	}
	// liveSurfaceInformed=false: the run that produced this evidence has already
	// finished, so an attached client is not streaming its events and cannot be
	// assumed to have shown anything.
	d.coordinator().routePendingNotification(ctx, identity, run.Channel, delivery.Message{
		TenantID: tenantID,
		PersonID: run.PersonID,
		TaskID:   run.TaskID,
		RunID:    runID,
		Content:  notice,
		Kind:     "skill_curation",
	}, false)
}

// skillCurationNoticeForRun reads the run's durable events and renders the
// notice, or "" when the pass reported nothing a person needs.
//
// The read must return the event TYPE and must not be narrowed to one type.
// This first used the run evidence reader, whose query hardcodes
// `type='evidence.recorded'` and does not select the type column at all, so it
// returned nothing for every curation outcome and the notice silently never
// fired — the exact invisibility this whole path exists to end.
func (d *Server) skillCurationNoticeForRun(ctx context.Context, tenantID, runID string) (string, *control.Run) {
	run, err := d.Control.GetRun(ctx, tenantID, runID)
	if err != nil || run == nil {
		return "", nil
	}
	events, err := d.Control.ListRunEvents(ctx, tenantID, run.PersonID, run.TaskID, runID, skillCurationNoticeEventScan)
	if err != nil {
		log.Debug("skill curation notice: event read failed", "run_id", runID, "error", err)
		return "", nil
	}
	return skillCurationNotice(events), run
}

// skillCurationNoticeEventScan bounds the tail a pass inspects. One curation
// writes at most a few events at the end of a run.
const skillCurationNoticeEventScan = 50

type skillCurationEventPayload struct {
	Name          string `json:"name"`
	VersionHash   string `json:"version_hash"`
	Promoted      bool   `json:"promoted"`
	BlockedReason string `json:"blocked_reason"`
}

// skillCurationNotice renders one bounded English line per outcome kind, or ""
// when the pass changed nothing a person needs to know. A promoted candidate
// produces two events with the same payload, so names are deduplicated.
func skillCurationNotice(events []control.Event) string {
	published := map[string]bool{}
	withheld := map[string]string{}
	var publishedOrder, withheldOrder []string
	for _, event := range events {
		switch event.Type {
		case "skill.version.promoted", "skill.candidate.created":
		default:
			continue
		}
		var payload skillCurationEventPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		name := strings.TrimSpace(payload.Name)
		if name == "" {
			continue
		}
		if payload.Promoted {
			if !published[name] {
				published[name] = true
				publishedOrder = append(publishedOrder, name)
			}
			delete(withheld, name)
			continue
		}
		reason := strings.TrimSpace(payload.BlockedReason)
		if reason == "" || published[name] {
			continue
		}
		if _, seen := withheld[name]; !seen {
			withheldOrder = append(withheldOrder, name)
		}
		withheld[name] = fmt.Sprintf("%s (%s, %s)", name, shortSkillVersionHash(payload.VersionHash), reason)
	}

	var lines []string
	if names := boundedSkillNames(publishedOrder); len(names) > 0 {
		lines = append(lines, "Learned Skill now active: "+strings.Join(names, ", ")+". Read it with /skills read <name>.")
	}
	// A name withheld earlier in the same pass and published later is a
	// publication, so the order list is filtered against the surviving map
	// rather than trusted on its own.
	remaining := make([]string, 0, len(withheldOrder))
	for _, name := range withheldOrder {
		if _, ok := withheld[name]; ok {
			remaining = append(remaining, name)
		}
	}
	if len(remaining) > 0 {
		details := make([]string, 0, len(remaining))
		for _, name := range boundedSkillNames(remaining) {
			if detail, ok := withheld[name]; ok {
				details = append(details, detail)
				continue
			}
			details = append(details, name)
		}
		lines = append(lines, "Skill candidate not published: "+strings.Join(details, "; ")+". Review with /skills candidate <version>.")
	}
	return strings.Join(lines, "\n")
}

func boundedSkillNames(names []string) []string {
	if len(names) <= skillCurationNoticeMaxNames {
		return names
	}
	bounded := append([]string{}, names[:skillCurationNoticeMaxNames]...)
	return append(bounded, fmt.Sprintf("and %d more", len(names)-skillCurationNoticeMaxNames))
}

func shortSkillVersionHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
