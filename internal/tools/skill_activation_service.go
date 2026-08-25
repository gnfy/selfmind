package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// ActivateSkillPackageInput contains control-plane routing data that must be
// identical regardless of whether activation came from model selection, slash,
// or a task binding. Resolution, guards, and candidate drift checks remain at
// their owning entrypoint; the immutable package-to-delivery transition lives
// here exactly once.
type ActivateSkillPackageInput struct {
	IdentityTenantID string
	ControlTenantID  string
	PersonID         string
	WorkspaceID      string
	RunID            string
	WorkUnitID       string
	ExecutionLane    string
	SkillKey         string
	ActivationSource string
	AttachmentMode   string
	ContentRef       string
	CreatedBy        string
	Budget           kernel.RuntimeContextBudget
}

type ActivatedSkillPackage struct {
	Activation *control.SkillActivation
	Context    *kernel.ActiveSkillContext
	Delivery   kernel.SkillMainDelivery
}

func ActivateSkillPackage(ctx context.Context, store *control.Store, pack SkillPackageSnapshot, input ActivateSkillPackageInput) (ActivatedSkillPackage, error) {
	if store == nil {
		return ActivatedSkillPackage{}, fmt.Errorf("control store is required")
	}
	if strings.TrimSpace(input.SkillKey) == "" || strings.TrimSpace(pack.Info.Name) == "" || strings.TrimSpace(pack.VersionHash) == "" || strings.TrimSpace(pack.PackageHash) == "" {
		return ActivatedSkillPackage{}, fmt.Errorf("resolved Skill package identity is incomplete")
	}
	files := make([]string, 0, len(pack.ResourceManifest))
	resources := make([]control.SkillPackageResource, 0, len(pack.ResourceManifest))
	for _, entry := range pack.ResourceManifest {
		files = append(files, entry.Path)
		resources = append(resources, control.SkillPackageResource{
			Path: entry.Path, ContentHash: entry.ContentHash, ContentBody: pack.ResourceBodies[entry.Path], Bytes: entry.Bytes,
		})
	}
	if err := store.RecordSkillPackageResources(ctx, input.ControlTenantID, input.SkillKey, pack.PackageHash, resources); err != nil {
		return ActivatedSkillPackage{}, err
	}
	budget := input.Budget
	if budget.SkillMainBytes <= 0 || budget.SkillMainTokens <= 0 {
		budget = kernel.DefaultRuntimeContextBudget()
	}
	delivery := kernel.BuildSkillMainDeliveryWithinBudget(pack.MainSource,
		kernel.ActiveSkillDeliveryBodyBudget(budget.SkillMainBytes, files), budget.SkillMainTokens)
	if err := kernel.ValidateSkillMainDeliveryReceipt(delivery.ContractVersion, delivery.Mode,
		delivery.Content, delivery.DeliveredHash, delivery.DeliveredBytes); err != nil {
		return ActivatedSkillPackage{}, err
	}
	manifestJSON, err := json.Marshal(pack.ResourceManifest)
	if err != nil {
		return ActivatedSkillPackage{}, err
	}
	activation, err := store.ActivateSkill(ctx, control.ActivateSkillInput{
		IdentityTenantID: input.IdentityTenantID, ControlTenantID: input.ControlTenantID,
		PersonID: input.PersonID, WorkspaceID: input.WorkspaceID, RunID: input.RunID,
		WorkUnitID: input.WorkUnitID, ExecutionLane: input.ExecutionLane,
		SkillKey: input.SkillKey, SkillName: pack.Info.Name, VersionHash: pack.VersionHash,
		PackageHash: pack.PackageHash, ActivationSource: input.ActivationSource,
		AttachmentMode: input.AttachmentMode, DeliveryContractVersion: delivery.ContractVersion,
		DeliveryMode: delivery.Mode, DeliveredMain: delivery.Content,
		DeliveredMainHash: delivery.DeliveredHash, DeliveredMainBytes: delivery.DeliveredBytes,
		ResourceManifestJSON: string(manifestJSON), ContentRef: input.ContentRef,
		ContentBody: pack.MainSource, CreatedBy: input.CreatedBy,
	})
	if err != nil {
		return ActivatedSkillPackage{}, err
	}
	// Existing activation rows are immutable baselines and win on idempotent
	// retries. Always rebuild the model context from the stored receipt. In
	// particular, a resource-only filesystem drift keeps the same main version
	// hash, so using the freshly resolved manifest here would mix new linked-file
	// names with the old activation package hash.
	delivery.ContractVersion = activation.DeliveryContractVersion
	delivery.Mode = activation.DeliveryMode
	delivery.Content = activation.DeliveredMain
	delivery.DeliveredHash = activation.DeliveredMainHash
	delivery.DeliveredBytes = activation.DeliveredMainBytes
	activeFiles := files
	if activation.DeliveryContractVersion > 0 {
		var activeManifest []SkillResourceManifestEntry
		if err := json.Unmarshal([]byte(activation.ResourceManifestJSON), &activeManifest); err != nil {
			return ActivatedSkillPackage{}, fmt.Errorf("decode active Skill resource manifest: %w", err)
		}
		activeFiles = make([]string, 0, len(activeManifest))
		for _, entry := range activeManifest {
			activeFiles = append(activeFiles, entry.Path)
		}
	}
	active := &kernel.ActiveSkillContext{
		ActivationID: activation.ID, WorkUnitID: activation.WorkUnitID, WorkUnitSequence: activation.WorkUnitSequence, Key: activation.SkillKey,
		Name: activation.SkillName, VersionHash: activation.VersionHash, Scope: pack.Info.Scope, Source: pack.Info.Source,
		Body: pack.MainSource, LinkedFiles: activeFiles, PackageHash: activation.PackageHash,
		DeliveryContractVersion: activation.DeliveryContractVersion, DeliveryMode: activation.DeliveryMode,
		DeliveredMain: activation.DeliveredMain, DeliveredHash: activation.DeliveredMainHash,
		DeliveredBytes: activation.DeliveredMainBytes,
	}
	return ActivatedSkillPackage{Activation: activation, Context: active, Delivery: delivery}, nil
}
