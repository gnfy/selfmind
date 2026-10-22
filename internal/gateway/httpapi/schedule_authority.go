package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/command"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/log"
)

// A schedule acts as, and delivers to, the person whose Run created it: it
// runs through one of that person's own bound accounts and delivers to that
// account's direct chat, never a chat shared with others. Its prompt is task
// text for a later Run, so it may not be a command. The cron tool binds a new
// job to such an endpoint, and every fire checks the stored job again, because
// a job stored before the tool checked it may name anything.
//
// A job stored by an older release may name no account at all: channel "cli"
// and no deliver_to, which that release ran as the "local" command-line
// account. Its cron tool let any Run store such a row, a Run started from a
// chat included, so the row belongs to the person the evidence shows created
// it, decided once and kept: evidence only ever disappears (a work-history
// reset, a moved or cleaned-up account), and its loss must never hand a job to
// someone else. See legacyScheduleOwner. The job runs as its owner's own
// command-line account. A job no one can be shown to own never runs; the
// installation's command-line users see it, are told, and may remove it or,
// on the local command line, adopt it.

// ScheduleAuthority implements cron.JobAuthority over the identity store.
type ScheduleAuthority struct {
	Control *control.Store
}

// NewScheduleAuthority binds model-created schedules to the person of the Run
// that creates them.
func NewScheduleAuthority(store *control.Store) *ScheduleAuthority {
	return &ScheduleAuthority{Control: store}
}

// Bind returns the endpoint a new job is stored with. With nothing named it is
// the endpoint the creating Run came from.
func (a *ScheduleAuthority) Bind(ctx context.Context, scope kernel.ToolInvocationScope, request cron.JobRequest) (cron.JobEndpoint, error) {
	if a == nil || a.Control == nil {
		return cron.JobEndpoint{}, fmt.Errorf("scheduling is unavailable: no identity store")
	}
	if command.LooksLikeCommand(strings.TrimPrefix(strings.TrimSpace(request.Prompt), CanaryPrefix)) {
		return cron.JobEndpoint{}, fmt.Errorf("the prompt begins with a SelfMind command; a job's prompt is task text for a later run, " +
			"and commands such as /approve run only from the person's own messages, so describe the task in words")
	}
	accounts, err := a.Control.ListAccountsByPerson(ctx, scope.ControlTenantID, scope.PersonID)
	if err != nil {
		return cron.JobEndpoint{}, err
	}
	account, err := scheduleAccount(accounts, scope.AccountID, strings.TrimSpace(request.Platform), strings.TrimSpace(request.DeliverTo))
	if err != nil {
		return cron.JobEndpoint{}, err
	}
	direct, err := directChat(ctx, a.Control, account.TenantID, account.PersonID, account.ID, account.PlatformUserID)
	if err != nil {
		return cron.JobEndpoint{}, err
	}
	channel, err := scheduleChannel(account, direct, request.Channel)
	if err != nil {
		return cron.JobEndpoint{}, err
	}
	return cron.JobEndpoint{TenantID: scope.ControlTenantID, Platform: account.Platform, DeliverTo: account.PlatformUserID, Channel: channel}, nil
}

// Standing reports whether the Run's person may see or change a stored job,
// and why the job does not run when it does not. A job no one can be shown to
// own is managed by the installation's command-line users.
func (a *ScheduleAuthority) Standing(ctx context.Context, scope kernel.ToolInvocationScope, job cron.CronJob) (cron.JobStanding, error) {
	if a == nil || a.Control == nil || job.TenantID != scope.ControlTenantID {
		return cron.JobStanding{}, nil
	}
	view, err := viewSchedule(ctx, a.Control, job, true)
	if err != nil {
		return cron.JobStanding{}, err
	}
	owned := view.owner != "" && view.owner == scope.PersonID
	if view.owner == "" {
		account, err := commandLineAccount(ctx, a.Control, scope.PersonID)
		if err != nil {
			return cron.JobStanding{}, err
		}
		owned = account != nil
	}
	return cron.JobStanding{Owned: owned, Blocked: view.refusal}, nil
}

// scheduleAccount picks the person's own account a new job acts through: the
// one the request named or, with nothing named, the one the creating Run came
// from. A platform alone means the request's own account on that platform, or
// the person's only one there; a deliver_to alone must name one account. An
// error names only the platforms the person's accounts are on, never their
// ids.
func scheduleAccount(accounts []control.Account, creatingAccountID, platform, deliverTo string) (control.Account, error) {
	var creating *control.Account
	for i := range accounts {
		if accounts[i].ID == creatingAccountID {
			creating = &accounts[i]
		}
	}
	if platform == "" && deliverTo == "" {
		if creating == nil {
			return control.Account{}, fmt.Errorf("this run has no endpoint of its own to deliver to; name the platform of one of your accounts (%s)", describePlatforms(accounts))
		}
		return *creating, nil
	}
	if deliverTo == "" && creating != nil && strings.EqualFold(creating.Platform, platform) {
		return *creating, nil
	}
	var matches []control.Account
	for _, account := range accounts {
		if (platform == "" || strings.EqualFold(account.Platform, platform)) && (deliverTo == "" || account.PlatformUserID == deliverTo) {
			matches = append(matches, account)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		named := strings.Trim(platform+":"+deliverTo, ":")
		return control.Account{}, fmt.Errorf("%s is not one of your accounts (%s); a job runs as and delivers to the person who creates it, "+
			"never another person or an unbound account", named, describePlatforms(accounts))
	default:
		return control.Account{}, fmt.Errorf("several of your accounts are on %s; ask the person which one and name it as deliver_to", matches[0].Platform)
	}
}

// scheduleChannel is the chat a job delivers to: its account's own direct
// chat, stored as the account's user id and resolved again at every fire. A
// CLI account has no chat; a channel shared with others is refused.
func scheduleChannel(account control.Account, direct, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if strings.EqualFold(account.Platform, "cli") {
		if requested == "" || strings.EqualFold(requested, "cli") {
			return "cli", nil
		}
		return "", fmt.Errorf("a CLI job has no chat to deliver to; omit channel")
	}
	if ownChat(account.Platform, account.PlatformUserID, direct, requested) {
		return account.PlatformUserID, nil
	}
	return "", fmt.Errorf("%s is not your own direct chat on %s; a job delivers only to the direct chat of the person it runs as, so omit channel",
		requested, account.Platform)
}

// ownChat reports whether a stored channel names an account's own direct
// chat: nothing, the platform, the user id, or the recorded direct chat.
func ownChat(platform, userID, direct, channel string) bool {
	channel = strings.TrimSpace(channel)
	return channel == "" || strings.EqualFold(channel, platform) || channel == userID || channel == direct
}

// describePlatforms names the platforms the person's accounts are on.
func describePlatforms(accounts []control.Account) string {
	var names []string
	seen := map[string]bool{}
	for _, account := range accounts {
		if name := strings.ToLower(account.Platform); !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "you have no bound accounts"
	}
	sort.Strings(names)
	return "yours are on " + strings.Join(names, ", ")
}

// directChatKey names the person setting that records an account's direct
// chat with SelfMind, for platforms whose chat id is not the user id.
func directChatKey(accountID string) string {
	return "im_direct_chat:" + accountID
}

// directChat is where SelfMind reaches one account alone: the direct chat
// recorded from that account's own messages, or else its user id, which is
// the direct chat on platforms that address a person by user id.
func directChat(ctx context.Context, store *control.Store, tenantID, personID, accountID, userID string) (string, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(personID) == "" {
		return userID, nil
	}
	recorded, err := store.GetPersonSetting(ctx, tenantID, personID, directChatKey(accountID))
	if err != nil {
		return "", err
	}
	return firstNonEmptyString(strings.TrimSpace(recorded), userID), nil
}

// scheduleView is how one stored job reads against the identity store.
type scheduleView struct {
	// owner is the person who sees and changes the job; empty when no one
	// can be shown to own it.
	owner string
	// account is the bound account a fire acts through and channel the chat
	// it delivers to; account is nil when the job does not run.
	account *control.IdentityContext
	channel string
	// refusal says why the job does not run. tell is the person told so in
	// their own direct chat, when someone may be.
	refusal string
	tell    *control.IdentityContext
}

// expectedScheduleCreator carries the checked, durable creator from the
// executor to admission. It is internal context authority, never wire input.
type expectedScheduleCreator struct {
	tenantID string
	personID string
}

type expectedScheduleCreatorKey struct{}

func withExpectedScheduleCreator(ctx context.Context, tenantID, personID string) context.Context {
	return context.WithValue(ctx, expectedScheduleCreatorKey{}, expectedScheduleCreator{tenantID: tenantID, personID: personID})
}

func scheduleCreatorFromContext(ctx context.Context) (expectedScheduleCreator, bool) {
	creator, ok := ctx.Value(expectedScheduleCreatorKey{}).(expectedScheduleCreator)
	return creator, ok
}

// viewSchedule reads a stored job without creating any identity: whose it is,
// whom a fire runs as, and where it delivers. With record set, the first
// ownership decision about an older row without a frozen creator is kept; the
// read-only doctor leaves it unset.
func viewSchedule(ctx context.Context, store *control.Store, job cron.CronJob, record bool) (scheduleView, error) {
	if namesNoAccount(job) {
		return viewUnaddressedSchedule(ctx, store, job, record)
	}
	owner := strings.TrimSpace(job.CreatorPersonID)
	if owner == "" {
		cli, err := installationCommandLine(ctx, store)
		if err != nil {
			return scheduleView{}, err
		}
		owner, err = legacyScheduleOwner(ctx, store, job, cli, record)
		if err != nil {
			return scheduleView{}, err
		}
	}
	if owner == "" {
		return scheduleView{refusal: "no one here can be shown to have created it"}, nil
	}
	platform := firstNonEmptyString(job.Platform, job.Channel, "cli")
	user := firstNonEmptyString(job.DeliverTo, "local")
	account, err := store.ResolveAccount(ctx, job.TenantID, platform, user)
	if err != nil {
		return scheduleView{}, err
	}
	if account == nil {
		return scheduleView{owner: owner, refusal: fmt.Sprintf("it names %s:%s, which is not bound to anyone here", platform, user)}, nil
	}
	if account.PersonID != owner {
		return scheduleView{owner: owner, refusal: "its account no longer belongs to the person who created it"}, nil
	}
	accounts, err := store.ListAccountsByPerson(ctx, account.TenantID, account.PersonID)
	if err != nil {
		return scheduleView{}, err
	}
	active := false
	for _, bound := range accounts {
		active = active || bound.ID == account.AccountID
	}
	if !active {
		return scheduleView{owner: account.PersonID, refusal: fmt.Sprintf("its account %s:%s is no longer active", platform, user)}, nil
	}
	if strings.EqualFold(account.Platform, "cli") {
		return scheduleView{owner: account.PersonID, account: account, channel: firstNonEmptyString(job.Channel, "cli")}, nil
	}
	direct, err := directChat(ctx, store, account.TenantID, account.PersonID, account.AccountID, account.PlatformUserID)
	if err != nil {
		return scheduleView{}, err
	}
	if ownChat(account.Platform, account.PlatformUserID, direct, job.Channel) {
		return scheduleView{owner: account.PersonID, account: account, channel: direct}, nil
	}
	return scheduleView{owner: account.PersonID, tell: account, refusal: fmt.Sprintf(
		"it would post to chat %s, which is not your own direct chat", strings.TrimSpace(job.Channel))}, nil
}

// viewUnaddressedSchedule reads a job that names no account, which an older
// release ran as the "local" command-line account.
func viewUnaddressedSchedule(ctx context.Context, store *control.Store, job cron.CronJob, record bool) (scheduleView, error) {
	cli, err := installationCommandLine(ctx, store)
	if err != nil {
		return scheduleView{}, err
	}
	owner := strings.TrimSpace(job.CreatorPersonID)
	if owner == "" && strings.TrimSpace(job.SystemKey) != "" {
		owner = cli.person
	} else if owner == "" {
		if owner, err = legacyScheduleOwner(ctx, store, job, cli, record); err != nil {
			return scheduleView{}, err
		}
	}
	if owner == "" {
		return scheduleView{refusal: "it names no account, and no one here can be shown to have created it"}, nil
	}
	account, err := commandLineAccount(ctx, store, owner)
	if err != nil {
		return scheduleView{}, err
	}
	if account == nil {
		return scheduleView{owner: owner, refusal: "it was stored for the command line, and you have no command-line account here"}, nil
	}
	return scheduleView{owner: owner, account: account, channel: firstNonEmptyString(job.Channel, "cli")}, nil
}

// Recorded decisions about an older row without a frozen creator.
const (
	legacyVerdictOwner     = "owner:"
	legacyVerdictAdopted   = "adopted:"
	legacyVerdictUnclaimed = "unclaimed"
)

// legacyScheduleKey identifies one older row for its recorded decision: the id
// alone could name a row of a recreated database. Empty for a job with no
// stored identity.
func legacyScheduleKey(job cron.CronJob) string {
	if job.ID <= 0 || job.CreatedAt.IsZero() {
		return ""
	}
	return fmt.Sprintf("legacy_schedule:%d:%d", job.ID, job.CreatedAt.Unix())
}

// verdictOwner is the person a recorded decision gives the job to, or "".
func verdictOwner(verdict string) string {
	for _, prefix := range []string{legacyVerdictAdopted, legacyVerdictOwner} {
		if strings.HasPrefix(verdict, prefix) {
			return strings.TrimPrefix(verdict, prefix)
		}
	}
	return ""
}

// legacyScheduleOwner is the person an older row without a frozen creator
// belongs to, or "" when no one can be shown to own it. The first decision
// is recorded and used from then on: the evidence behind it — Run receipts,
// the set of people with accounts — only shrinks over time, and a smaller set
// of evidence must not move a job to the installation's command line. A
// person's explicit adoption on the local command line replaces it.
func legacyScheduleOwner(ctx context.Context, store *control.Store, job cron.CronJob, cli commandLineIdentity, record bool) (string, error) {
	key := legacyScheduleKey(job)
	if key != "" {
		verdict, err := store.LegacyScheduleVerdict(ctx, job.TenantID, key)
		if err != nil || verdict != "" {
			return verdictOwner(verdict), err
		}
	}
	owner, err := decideLegacyScheduleOwner(ctx, store, job, cli)
	if err != nil || key == "" || !record {
		return owner, err
	}
	verdict := legacyVerdictUnclaimed
	if owner != "" {
		verdict = legacyVerdictOwner + owner
	}
	kept, err := store.RecordLegacyScheduleVerdict(ctx, job.TenantID, key, verdict)
	return verdictOwner(kept), err
}

// decideLegacyScheduleOwner reads the evidence about an older row: the person
// whose own Run recorded the cron tool's receipt for it, or else the
// installation's command-line person when no one else has ever held an
// account here, or when the row is the bare liveness canary, whose turn is
// fixed text. A receipt from the cli:local ghost is no evidence of its own:
// the older executor ran other people's jobs as that ghost.
func decideLegacyScheduleOwner(ctx context.Context, store *control.Store, job cron.CronJob, cli commandLineIdentity) (string, error) {
	creator, err := scheduleCreator(ctx, store, job, cli)
	if err != nil || creator != "" {
		return creator, err
	}
	if cli.person == "" {
		return "", nil
	}
	if isBareCanary(job.Prompt) {
		return cli.person, nil
	}
	alone, err := onlyCommandLineHoldsAccounts(ctx, store, cli)
	if err != nil || !alone {
		return "", err
	}
	return cli.person, nil
}

// commandLineIdentity is the person this installation's command line acts as
// and, when it is someone else, the person of cli:local: the person an older
// executor created when it ran a job that named no account.
type commandLineIdentity struct {
	person string
	ghost  string
}

// installationCommandLine finds the one person holding active command-line
// accounts other than the "local" fallback or, when nobody does, the person of
// cli:local. Several such persons leave it unknown, with any cli:local person
// still a ghost.
func installationCommandLine(ctx context.Context, store *control.Store) (commandLineIdentity, error) {
	accounts, err := store.ActiveAccountsOnPlatform(ctx, "cli")
	if err != nil {
		return commandLineIdentity{}, err
	}
	local, named := "", map[string]bool{}
	for _, account := range accounts {
		if account.PlatformUserID == "local" {
			local = account.PersonID
		} else {
			named[account.PersonID] = true
		}
	}
	identity := commandLineIdentity{}
	if local != "" && !named[local] {
		identity.ghost = local
	}
	switch len(named) {
	case 0:
		return commandLineIdentity{person: local}, nil
	case 1:
		for person := range named {
			identity.person = person
		}
	}
	return identity, nil
}

// scheduleReceiptWindow bounds when the creating Run recorded the cron tool's
// receipt, around the job's creation second.
const (
	scheduleReceiptBefore = time.Minute
	scheduleReceiptAfter  = 10 * time.Minute
)

// scheduleCreator is the person whose own Run recorded the cron tool's receipt
// for this job, or "" when no single person did or when only the cli:local
// ghost did. Receipts from Runs the daemon started never count.
func scheduleCreator(ctx context.Context, store *control.Store, job cron.CronJob, cli commandLineIdentity) (string, error) {
	if job.ID <= 0 || job.CreatedAt.IsZero() {
		return "", nil
	}
	persons, err := store.PersonsRecordingToolResult(ctx, "cron", cron.AddedReceipt(job.ID, job.Name),
		job.CreatedAt.Add(-scheduleReceiptBefore), job.CreatedAt.Add(scheduleReceiptAfter))
	if err != nil || len(persons) != 1 || (cli.ghost != "" && persons[0] == cli.ghost) {
		return "", err
	}
	return persons[0], nil
}

// onlyCommandLineHoldsAccounts reports whether everyone who has ever held an
// account here, in any tenant, is the command-line person or its cli:local
// ghost: then no one else could have stored the job. Accounts outlive a
// work-history reset; Runs do not.
func onlyCommandLineHoldsAccounts(ctx context.Context, store *control.Store, cli commandLineIdentity) (bool, error) {
	persons, err := store.PersonsWithAccounts(ctx, 3)
	if err != nil {
		return false, err
	}
	for _, person := range persons {
		if person != cli.person && person != cli.ghost {
			return false, nil
		}
	}
	return true, nil
}

// isBareCanary reports whether a prompt is the liveness canary with no task of
// its own, which the executor replaces with a fixed READY check.
func isBareCanary(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	return strings.HasPrefix(prompt, CanaryPrefix) && strings.TrimSpace(strings.TrimPrefix(prompt, CanaryPrefix)) == ""
}

// commandLineAccount is the person's own command-line account a job that names
// none acts through: the most recently seen one other than the "local"
// fallback, or that fallback. Nil when the person has none.
func commandLineAccount(ctx context.Context, store *control.Store, personID string) (*control.IdentityContext, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, nil
	}
	accounts, err := store.AccountsOfPerson(ctx, personID)
	if err != nil {
		return nil, err
	}
	var chosen *control.Account
	for i := range accounts {
		account := &accounts[i]
		if !strings.EqualFold(account.Platform, "cli") {
			continue
		}
		switch {
		case chosen == nil:
			chosen = account
		case chosen.PlatformUserID == "local" && account.PlatformUserID != "local":
			chosen = account
		case (chosen.PlatformUserID == "local") == (account.PlatformUserID == "local") && account.LastSeenAt > chosen.LastSeenAt:
			chosen = account
		}
	}
	if chosen == nil {
		return nil, nil
	}
	return &control.IdentityContext{TenantID: chosen.TenantID, PersonID: chosen.PersonID, AccountID: chosen.ID,
		Platform: chosen.Platform, PlatformUserID: chosen.PlatformUserID, DisplayName: chosen.DisplayName}, nil
}

// resolveScheduledFire checks a stored job before it fires.
func (d *Server) resolveScheduledFire(ctx context.Context, job cron.CronJob) (scheduleView, error) {
	if d == nil || d.Control == nil {
		return scheduleView{}, fmt.Errorf("scheduled work needs an identity store")
	}
	return viewSchedule(ctx, d.Control, job, true)
}

// refusedScheduleNotice says why a job did not run and, when it was turned
// off, that it stays off until the person changes it.
func refusedScheduleNotice(job cron.CronJob, view scheduleView, turnedOff bool) string {
	text := fmt.Sprintf("Scheduled job %q was not run: %s.", job.Name, view.refusal)
	if turnedOff {
		text = fmt.Sprintf("Scheduled job %q was not run and is now turned off: %s.", job.Name, view.refusal)
	}
	if view.tell != nil {
		text += " A schedule delivers only to the person it runs as; ask me to add it again without a chat to have it delivered here, or to remove it."
	}
	return text
}

// noticeRefusedSchedule records a refused fire in the daemon log and tells the
// person the job runs as, once per job and chat, in their own direct chat.
// With no such person nobody may be told.
func (d *Server) noticeRefusedSchedule(ctx context.Context, job cron.CronJob, view scheduleView, notice string, turnedOff bool) {
	log.Warn("cron: scheduled job refused", "job_id", job.ID, "job", job.Name, "reason", view.refusal, "turned_off", turnedOff)
	tell := view.tell
	if tell == nil || d.Delivery == nil {
		return
	}
	channel, err := directChat(ctx, d.Control, tell.TenantID, tell.PersonID, tell.AccountID, tell.PlatformUserID)
	if err != nil {
		log.Warn("cron: refusal notice has no direct chat", "job_id", job.ID, "error", err)
		return
	}
	if err := d.Delivery.EnqueueAndTry(ctx, delivery.Message{
		TenantID: tell.TenantID, PersonID: tell.PersonID, Platform: tell.Platform,
		PlatformUserID: tell.PlatformUserID, Channel: channel, Content: notice,
		LogicalKey: fmt.Sprintf("schedule-refused:%d:%s:%s", job.ID, job.Name, job.Channel),
	}); err != nil {
		log.Warn("cron: refusal notice was not delivered", "job_id", job.ID, "error", err)
	}
}
