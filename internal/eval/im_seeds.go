package eval

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/tools"
)

// SeedAccount is one more platform account of the case person.
type SeedAccount struct {
	Platform       string `yaml:"platform" json:"platform"`
	PlatformUserID string `yaml:"platform_user_id" json:"platform_user_id"`
}

// SeedTurnChoice is a pending multi-Run choice: the numbered question the
// gateway asks one account on one channel when several Runs could own an
// ordinary message. Choosing a number submits Request to that option.
type SeedTurnChoice struct {
	// Platform and PlatformUserID name the account that was asked; empty means
	// the case user on the eval platform.
	Platform       string             `yaml:"platform,omitempty" json:"platform,omitempty"`
	PlatformUserID string             `yaml:"platform_user_id,omitempty" json:"platform_user_id,omitempty"`
	Channel        string             `yaml:"channel" json:"channel"`
	Request        string             `yaml:"request" json:"request"`
	Options        []SeedChoiceOption `yaml:"options" json:"options"`
}

// SeedChoiceOption is one numbered answer. Keys follow list order from 1.
type SeedChoiceOption struct {
	// Action is steer or resume (both need ParkedRun) or new.
	Action string `yaml:"action" json:"action"`
	// ParkedRun is a 1-based setup.task.parked_runs ordinal.
	ParkedRun int    `yaml:"parked_run,omitempty" json:"parked_run,omitempty"`
	Label     string `yaml:"label,omitempty" json:"label,omitempty"`
}

// SeedSentMessage is an IM message the daemon delivered with a platform
// message id. A turn replying to that id carries the edges recorded here.
type SeedSentMessage struct {
	Platform       string `yaml:"platform,omitempty" json:"platform,omitempty"`
	PlatformUserID string `yaml:"platform_user_id,omitempty" json:"platform_user_id,omitempty"`
	Channel        string `yaml:"channel" json:"channel"`
	MessageID      string `yaml:"message_id" json:"message_id"`
	// ParkedRun links the message to a seeded Run (1-based ordinal).
	ParkedRun int `yaml:"parked_run,omitempty" json:"parked_run,omitempty"`
	// ApprovalID and ClarifyID are literal ids. The harness does not create
	// the approval or question, so a case can model a stale or missing target.
	ApprovalID string `yaml:"approval_id,omitempty" json:"approval_id,omitempty"`
	ClarifyID  string `yaml:"clarify_id,omitempty" json:"clarify_id,omitempty"`
	Content    string `yaml:"content,omitempty" json:"content,omitempty"`
}

// accountRef identifies one platform account.
type accountRef struct {
	platform string
	user     string
}

// caseUser is the platform user id of the case person on every platform.
func (c *Case) caseUser() string {
	return "eval-" + c.ID
}

// accountFor resolves a seed or turn account, defaulting to the case user on
// the eval platform.
func (c *Case) accountFor(platform, user string) accountRef {
	return accountRef{platform: firstNonEmpty(platform, "eval"), user: firstNonEmpty(user, c.caseUser())}
}

// personAccounts lists the accounts bound to the case person besides its eval
// account: the case user on every platform a turn or seed names, and each
// setup.accounts entry.
func (c *Case) personAccounts() []accountRef {
	seen := map[accountRef]bool{c.accountFor("", ""): true}
	var out []accountRef
	add := func(ref accountRef) {
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	for _, turn := range c.Turns {
		if ref := c.accountFor(turn.Platform, turn.PlatformUserID); ref.user == c.caseUser() {
			add(ref)
		}
	}
	if c.Setup == nil {
		return out
	}
	for _, account := range c.Setup.Accounts {
		add(accountRef{platform: account.Platform, user: account.PlatformUserID})
	}
	for _, seed := range c.Setup.TurnChoices {
		add(c.accountFor(seed.Platform, seed.PlatformUserID))
	}
	for _, seed := range c.Setup.SentMessages {
		add(c.accountFor(seed.Platform, seed.PlatformUserID))
	}
	return out
}

// bindPersonAccounts links every person account to the case identity through
// the production binding path and returns all of them, the eval account
// included.
func bindPersonAccounts(ctx context.Context, store *control.Store, c *Case, identity *control.IdentityContext) (map[accountRef]*control.IdentityContext, error) {
	accounts := map[accountRef]*control.IdentityContext{c.accountFor("", ""): identity}
	for _, ref := range c.personAccounts() {
		bound, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, ref.platform, ref.user, "SelfMind Eval")
		if err != nil {
			return nil, fmt.Errorf("bind %s account %s: %w", ref.platform, ref.user, err)
		}
		accounts[ref] = bound
	}
	return accounts, nil
}

// validateEndpointSeeds checks the endpoint seeds against the rest of the
// setup before any state is written.
func (c *Case) validateEndpointSeeds() error {
	if c.Setup == nil {
		return nil
	}
	setup := c.Setup
	parked := 0
	if setup.Task != nil {
		parked = len(setup.Task.ParkedRuns)
	}
	bound := map[accountRef]bool{c.accountFor("", ""): true}
	for i := range setup.Accounts {
		account := &setup.Accounts[i]
		account.Platform = strings.ToLower(strings.TrimSpace(account.Platform))
		account.PlatformUserID = strings.TrimSpace(account.PlatformUserID)
		if !validEndpointName(account.Platform) || account.PlatformUserID == "" {
			return fmt.Errorf("setup.accounts[%d] needs a platform name and a platform_user_id", i)
		}
		bound[accountRef{platform: account.Platform, user: account.PlatformUserID}] = true
	}
	// The case user is the person's own account on any platform; another user
	// must have been bound explicitly.
	ownAccount := func(field, platform, user string) error {
		ref := c.accountFor(platform, user)
		if ref.user != c.caseUser() && !bound[ref] {
			return fmt.Errorf("%s names account %s/%s, which is not the case person's; add it to setup.accounts", field, ref.platform, ref.user)
		}
		return nil
	}
	for i := range setup.TurnChoices {
		seed := &setup.TurnChoices[i]
		field := fmt.Sprintf("setup.turn_choices[%d]", i)
		seed.Platform = strings.ToLower(strings.TrimSpace(seed.Platform))
		seed.PlatformUserID = strings.TrimSpace(seed.PlatformUserID)
		seed.Channel = strings.TrimSpace(seed.Channel)
		seed.Request = strings.TrimSpace(seed.Request)
		if seed.Platform != "" && !validEndpointName(seed.Platform) {
			return fmt.Errorf("%s.platform %q is not a platform name", field, seed.Platform)
		}
		if seed.Channel == "" || seed.Request == "" {
			return fmt.Errorf("%s needs a channel and the original request", field)
		}
		if err := ownAccount(field, seed.Platform, seed.PlatformUserID); err != nil {
			return err
		}
		if len(seed.Options) < 2 || len(seed.Options) > 8 {
			return fmt.Errorf("%s needs 2 to 8 options", field)
		}
		for j := range seed.Options {
			option := &seed.Options[j]
			option.Action = strings.ToLower(strings.TrimSpace(option.Action))
			switch option.Action {
			case "steer", "resume":
				if option.ParkedRun < 1 || option.ParkedRun > parked {
					return fmt.Errorf("%s.options[%d]: %s needs parked_run naming a setup.task.parked_runs entry", field, j, option.Action)
				}
			case "new":
				if option.ParkedRun != 0 {
					return fmt.Errorf("%s.options[%d]: new work cannot name a parked_run", field, j)
				}
			default:
				return fmt.Errorf("%s.options[%d]: action must be steer, resume or new", field, j)
			}
		}
	}
	seenMessages := map[string]bool{}
	for i := range setup.SentMessages {
		seed := &setup.SentMessages[i]
		field := fmt.Sprintf("setup.sent_messages[%d]", i)
		seed.Platform = strings.ToLower(strings.TrimSpace(seed.Platform))
		seed.PlatformUserID = strings.TrimSpace(seed.PlatformUserID)
		seed.Channel = strings.TrimSpace(seed.Channel)
		seed.MessageID = strings.TrimSpace(seed.MessageID)
		if seed.Platform != "" && !validEndpointName(seed.Platform) {
			return fmt.Errorf("%s.platform %q is not a platform name", field, seed.Platform)
		}
		if seed.Channel == "" || seed.MessageID == "" {
			return fmt.Errorf("%s needs a channel and a message_id", field)
		}
		if err := ownAccount(field, seed.Platform, seed.PlatformUserID); err != nil {
			return err
		}
		if seed.ParkedRun < 0 || seed.ParkedRun > parked {
			return fmt.Errorf("%s.parked_run must name a setup.task.parked_runs entry", field)
		}
		key := firstNonEmpty(seed.Platform, "eval") + "\x00" + seed.Channel + "\x00" + seed.MessageID
		if seenMessages[key] {
			return fmt.Errorf("%s repeats message %s on channel %s", field, seed.MessageID, seed.Channel)
		}
		seenMessages[key] = true
	}
	return nil
}

// endpointSeedContext carries what the endpoint seeds refer to.
type endpointSeedContext struct {
	accounts    map[accountRef]*control.IdentityContext
	workspace   string
	workspaceID string
	taskID      string
	parkedRuns  []string
}

// applyEndpointSeeds writes the pending choices and delivered IM messages and
// returns the ids of the seeded choices, in order.
func applyEndpointSeeds(ctx context.Context, store *control.Store, c *Case, seeds endpointSeedContext) ([]string, error) {
	if c.Setup == nil || store == nil {
		return nil, nil
	}
	var roots []executionenv.RootBinding
	if seeds.workspaceID != "" {
		if workspace, err := store.GetWorkspace(ctx, seeds.accounts[c.accountFor("", "")].TenantID, seeds.workspaceID); err == nil && workspace != nil {
			roots = httpapi.WorkspaceRootBindings(workspace)
		}
	}
	var choiceIDs []string
	for i, seed := range c.Setup.TurnChoices {
		ref := c.accountFor(seed.Platform, seed.PlatformUserID)
		account := seeds.accounts[ref]
		// The request an eval turn from that account would have sent, stored by
		// the gateway's own snapshot writer.
		request, err := httpapi.MultiRunChoiceRequest(api.MessageRequest{
			Content: seed.Request, Platform: ref.platform, PlatformUserID: ref.user, Channel: seed.Channel,
			WorkspaceID: seeds.workspaceID, ExecutionRoots: roots, ClientCWD: seeds.workspace,
			ApprovalMode: firstNonEmpty(c.ApprovalMode, string(tools.ApprovalFullAuto)),
		})
		if err != nil {
			return nil, err
		}
		options := make([]control.TurnChoiceOption, 0, len(seed.Options))
		for j, option := range seed.Options {
			choice := control.TurnChoiceOption{Key: fmt.Sprint(j + 1), Label: option.Label, Action: option.Action}
			if option.ParkedRun > 0 {
				choice.TaskID, choice.RunID = seeds.taskID, seeds.parkedRuns[option.ParkedRun-1]
			}
			if choice.Label == "" {
				choice.Label = defaultChoiceLabel(choice)
			}
			options = append(options, choice)
		}
		created, err := store.CreatePendingTurnChoice(ctx, control.PendingTurnChoiceCreate{
			TenantID: account.TenantID, PersonID: account.PersonID, AccountID: account.AccountID,
			Channel: seed.Channel, RequestJSON: request, Options: options,
		})
		if err != nil {
			return nil, fmt.Errorf("seed turn choice %d: %w", i, err)
		}
		choiceIDs = append(choiceIDs, created.ID)
	}
	for i, seed := range c.Setup.SentMessages {
		if err := seedSentMessage(ctx, store, c, seeds, seed); err != nil {
			return nil, fmt.Errorf("seed sent message %d: %w", i, err)
		}
	}
	return choiceIDs, nil
}

func defaultChoiceLabel(option control.TurnChoiceOption) string {
	switch option.Action {
	case "new":
		return "This is new work"
	case "resume":
		return "Resume " + option.RunID
	default:
		return "Update " + option.RunID
	}
}

// seedSentMessage records one delivery exactly as a platform sender does:
// enqueue, claim, then mark it sent with the platform's message id, which is
// what links a native reply to its edges.
func seedSentMessage(ctx context.Context, store *control.Store, c *Case, seeds endpointSeedContext, seed SeedSentMessage) error {
	ref := c.accountFor(seed.Platform, seed.PlatformUserID)
	account := seeds.accounts[ref]
	delivery := control.Delivery{
		TenantID: account.TenantID, PersonID: account.PersonID, Platform: ref.platform,
		PlatformUserID: ref.user, Channel: seed.Channel, ApprovalID: seed.ApprovalID, ClarifyID: seed.ClarifyID,
		Content: firstNonEmpty(seed.Content, "SelfMind delivered message "+seed.MessageID), Kind: "final_result",
	}
	switch {
	case seed.ApprovalID != "":
		delivery.Kind = "approval"
	case seed.ClarifyID != "":
		delivery.Kind = "clarify"
	}
	if seed.ParkedRun > 0 {
		delivery.TaskID, delivery.RunID = seeds.taskID, seeds.parkedRuns[seed.ParkedRun-1]
	}
	queued, err := store.EnqueueDelivery(ctx, delivery)
	if err != nil {
		return err
	}
	if claimed, err := store.ClaimDelivery(ctx, queued.ID); err != nil || !claimed {
		return fmt.Errorf("claim delivery %s: claimed=%t err=%v", queued.ID, claimed, err)
	}
	return store.MarkDeliverySentWithNativeID(ctx, queued.ID, seed.MessageID)
}
