package promptassets

import "sort"

// CatalogVersion identifies the local prompt-file grammar. It is deliberately
// independent from individual locked prompt contracts, which remain owned by
// the code that parses and applies each model response.
const CatalogVersion = 1

const (
	FileAgent            = "agent"
	FileMemoryExtract    = "memory_extract"
	FileBackgroundReview = "background_review"
	FileSkillCurator     = "skill_curator"
	FileSummarizer       = "summarizer"
	FileSemanticRecall   = "semantic_recall"
)

const (
	SectionPersona                 = "Persona"
	SectionWorkingStyle            = "Working Style"
	SectionVerificationPreferences = "Verification Preferences"
	SectionProgressUpdates         = "Progress Updates"
	SectionFrontendUI              = "Frontend and UI"
	SectionLearningPreferences     = "Learning Preferences"

	SectionPostRunAnalysis      = "Post-run Analysis"
	SectionBatchPostRunAnalysis = "Batch Post-run Analysis"
	SectionConsolidation        = "Consolidation"
	SectionLearningFocus        = "Learning Focus"
	SectionReviewStyle          = "Review Style"
	SectionCreationQuality      = "Creation Quality"
	SectionRepairQuality        = "Repair Quality"
	SectionNamingLanguage       = "Naming and Language"
	SectionSummaryPriorities    = "Summary Priorities"
	SectionLanguageDetail       = "Language and Detail"
	SectionExpansionGuidance    = "Expansion Guidance"
	SectionDomainVocabulary     = "Domain Vocabulary"
)

type SectionPolicy struct {
	Name        string
	MaxBytes    int
	AllowOff    bool
	Replace     bool
	Injection   string
	Stable      bool
	Description string
}

// EditPolicyLabel is the user-facing customization contract. Replaceable
// sections substitute their built-in presentation slot; append-only sections
// preserve the code-owned base and add operator guidance after it.
func (p SectionPolicy) EditPolicyLabel() string {
	if p.Replace {
		if p.AllowOff {
			return "replaceable; off allowed"
		}
		return "replaceable; off not allowed"
	}
	return "append-only; locked base preserved; off not allowed"
}

func (p SectionPolicy) PlacementLabel() string {
	if p.Stable {
		return "stable prompt prefix"
	}
	return "conditional/role-local prompt"
}

type FileSpec struct {
	ID           string
	Title        string
	RelativePath string
	MaxBytes     int
	Sections     []SectionPolicy
}

var catalog = []FileSpec{
	{
		ID: FileAgent, Title: "SelfMind Agent", RelativePath: "agent.md", MaxBytes: 12 * 1024,
		Sections: []SectionPolicy{
			{Name: SectionPersona, MaxBytes: 4 * 1024, AllowOff: true, Replace: true, Injection: "primary foreground turns at the persona slot; delegated agents retain their role identity", Stable: true, Description: "Global persona, language, and communication style."},
			{Name: SectionWorkingStyle, MaxBytes: 4 * 1024, Injection: "primary and delegated turns after the built-in work-quality floor", Stable: true, Description: "Additional cross-project working preferences."},
			{Name: SectionVerificationPreferences, MaxBytes: 4 * 1024, Injection: "primary and delegated turns after the built-in verification floor", Stable: true, Description: "Additional verification preferences; cannot disable the built-in evidence floor."},
			{Name: SectionProgressUpdates, MaxBytes: 2 * 1024, AllowOff: true, Replace: true, Injection: "primary foreground turns; applies only before tool batches", Stable: true, Description: "Replace or disable progress narration guidance."},
			{Name: SectionFrontendUI, MaxBytes: 4 * 1024, AllowOff: true, Replace: true, Injection: "primary and delegated turns; semantically conditional on user-facing interface work", Stable: true, Description: "Replace or disable user-facing interface quality guidance; the applicability boundary remains code-owned."},
			{Name: SectionLearningPreferences, MaxBytes: 4 * 1024, Injection: "primary foreground turns when a durable learning surface is available", Stable: false, Description: "Additional learning preferences; cannot relax memory or Skill governance."},
		},
	},
	{
		ID: FileMemoryExtract, Title: "Memory Extract", RelativePath: "background/memory_extract.md", MaxBytes: 8 * 1024,
		Sections: []SectionPolicy{
			{Name: SectionPostRunAnalysis, MaxBytes: 4 * 1024, Injection: "memory_extract.post_run", Description: "Quality guidance appended to the locked single-run contract."},
			{Name: SectionBatchPostRunAnalysis, MaxBytes: 4 * 1024, Injection: "memory_extract.post_run_batch", Description: "Quality guidance appended to the locked batch contract."},
			{Name: SectionConsolidation, MaxBytes: 4 * 1024, Injection: "memory_extract.consolidation", Description: "Quality guidance appended to the locked consolidation contract."},
		},
	},
	{
		ID: FileBackgroundReview, Title: "Background Review", RelativePath: "background/background_review.md", MaxBytes: 8 * 1024,
		Sections: []SectionPolicy{
			{Name: SectionLearningFocus, MaxBytes: 4 * 1024, Injection: "background_review system guidance", Description: "Additional durable-learning priorities."},
			{Name: SectionReviewStyle, MaxBytes: 4 * 1024, Injection: "background_review system guidance", Description: "Additional review and summary style preferences."},
		},
	},
	{
		ID: FileSkillCurator, Title: "Skill Curator", RelativePath: "background/skill_curator.md", MaxBytes: 8 * 1024,
		Sections: []SectionPolicy{
			{Name: SectionCreationQuality, MaxBytes: 4 * 1024, Injection: "skill_curator CREATE guidance", Description: "Additional quality guidance for new Skills."},
			{Name: SectionRepairQuality, MaxBytes: 4 * 1024, Injection: "skill_curator PATCH guidance", Description: "Additional quality guidance for narrow repairs."},
			{Name: SectionNamingLanguage, MaxBytes: 4 * 1024, Injection: "skill_curator naming guidance", Description: "Naming, terminology, and language preferences."},
		},
	},
	{
		ID: FileSummarizer, Title: "Summarizer", RelativePath: "background/summarizer.md", MaxBytes: 8 * 1024,
		Sections: []SectionPolicy{
			{Name: SectionSummaryPriorities, MaxBytes: 4 * 1024, Injection: "summarizer locked-system-contract initial and update calls", Description: "Additional summary priorities; cannot replace resume-critical sections."},
			{Name: SectionLanguageDetail, MaxBytes: 4 * 1024, Injection: "summarizer locked-system-contract initial and update calls", Description: "Language and detail preferences."},
		},
	},
	{
		ID: FileSemanticRecall, Title: "Semantic Recall", RelativePath: "background/semantic_recall.md", MaxBytes: 8 * 1024,
		Sections: []SectionPolicy{
			{Name: SectionExpansionGuidance, MaxBytes: 4 * 1024, Injection: "semantic_recall query expansion", Description: "Additional query-expansion guidance."},
			{Name: SectionDomainVocabulary, MaxBytes: 4 * 1024, Injection: "semantic_recall query expansion", Description: "Domain vocabulary and multilingual terminology."},
		},
	},
}

func Catalog() []FileSpec {
	out := make([]FileSpec, len(catalog))
	for i, spec := range catalog {
		out[i] = spec
		out[i].Sections = append([]SectionPolicy(nil), spec.Sections...)
	}
	return out
}

func Spec(id string) (FileSpec, bool) {
	for _, spec := range catalog {
		if spec.ID == id {
			copy := spec
			copy.Sections = append([]SectionPolicy(nil), spec.Sections...)
			return copy, true
		}
	}
	return FileSpec{}, false
}

func IDs() []string {
	ids := make([]string, 0, len(catalog))
	for _, spec := range catalog {
		ids = append(ids, spec.ID)
	}
	sort.Strings(ids)
	return ids
}
