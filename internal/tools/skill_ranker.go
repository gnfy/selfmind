package tools

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

const (
	skillBM25K1                = 1.2
	skillBM25NameWeight        = 4.0
	skillBM25DescriptionWeight = 1.0
	skillBM25NameB             = 0.30
	skillBM25DescriptionB      = 0.75
	skillBM25MaxQueryTerms     = 64
	maxSkillSearchResults      = 10
)

var skillBM25StopWords = map[string]bool{
	"a": true, "an": true, "and": true, "for": true, "from": true,
	"in": true, "of": true, "on": true, "or": true, "the": true,
	"to": true, "use": true, "using": true, "when": true, "with": true,
}

type skillBM25Document struct {
	info              SkillInfo
	nameTerms         map[string]int
	descriptionTerms  map[string]int
	nameLength        int
	descriptionLength int
}

type skillBM25Corpus struct {
	documents                []skillBM25Document
	documentFrequency        map[string]int
	averageNameLength        float64
	averageDescriptionLength float64
}

type rankedSkillMetadata struct {
	info  SkillInfo
	score float64
	exact bool
}

// rankSkillsBM25F ranks compact Skill metadata only. Full SKILL.md bodies stay
// outside discovery, and callers remain responsible for lifecycle filtering
// and result limits appropriate to their surface.
func rankSkillsBM25F(query string, skills []SkillInfo, limit int) []SkillInfo {
	if limit <= 0 || len(skills) == 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	queryTerms := uniqueSkillTerms(skillBM25Tokens(query, skillBM25MaxQueryTerms))
	queryCJKTerms := cjkSkillTerms(queryTerms)
	corpus := buildSkillBM25Corpus(skills)
	ranked := make([]rankedSkillMetadata, 0, len(corpus.documents))
	documentCount := float64(len(corpus.documents))
	for _, document := range corpus.documents {
		score := 0.0
		for _, term := range queryTerms {
			df := corpus.documentFrequency[term]
			if df == 0 {
				continue
			}
			nameNorm := 1 - skillBM25NameB + skillBM25NameB*float64(document.nameLength)/corpus.averageNameLength
			descriptionNorm := 1 - skillBM25DescriptionB + skillBM25DescriptionB*float64(document.descriptionLength)/corpus.averageDescriptionLength
			weightedTF := skillBM25NameWeight*float64(document.nameTerms[term])/nameNorm +
				skillBM25DescriptionWeight*float64(document.descriptionTerms[term])/descriptionNorm
			if weightedTF == 0 {
				continue
			}
			idf := math.Log(1 + (documentCount-float64(df)+0.5)/(float64(df)+0.5))
			score += idf * (weightedTF * (skillBM25K1 + 1) / (weightedTF + skillBM25K1))
		}
		exact := skillCanonicalNameMentioned(query, document.info.Name)
		if !exact && len(queryCJKTerms) >= 3 && matchedSkillTerms(queryTerms, document) < 2 {
			continue
		}
		if score > 0 || exact {
			ranked = append(ranked, rankedSkillMetadata{info: document.info, score: score, exact: exact})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].exact != ranked[j].exact {
			return ranked[i].exact
		}
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].info.Scope != ranked[j].info.Scope {
			return ranked[i].info.Scope == SkillScopeWorkspace
		}
		return ranked[i].info.Name < ranked[j].info.Name
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]SkillInfo, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.info)
	}
	return out
}

func buildSkillBM25Corpus(skills []SkillInfo) skillBM25Corpus {
	corpus := skillBM25Corpus{documentFrequency: map[string]int{}}
	for _, info := range skills {
		nameTokens := skillBM25Tokens(info.Name, 0)
		descriptionTokens := skillBM25Tokens(info.Description, 0)
		document := skillBM25Document{
			info: info, nameTerms: skillTermFrequencies(nameTokens),
			descriptionTerms: skillTermFrequencies(descriptionTokens),
			nameLength:       len(nameTokens), descriptionLength: len(descriptionTokens),
		}
		seen := map[string]bool{}
		for term := range document.nameTerms {
			seen[term] = true
		}
		for term := range document.descriptionTerms {
			seen[term] = true
		}
		for term := range seen {
			corpus.documentFrequency[term]++
		}
		corpus.averageNameLength += float64(document.nameLength)
		corpus.averageDescriptionLength += float64(document.descriptionLength)
		corpus.documents = append(corpus.documents, document)
	}
	if len(corpus.documents) > 0 {
		corpus.averageNameLength /= float64(len(corpus.documents))
		corpus.averageDescriptionLength /= float64(len(corpus.documents))
	}
	if corpus.averageNameLength == 0 {
		corpus.averageNameLength = 1
	}
	if corpus.averageDescriptionLength == 0 {
		corpus.averageDescriptionLength = 1
	}
	return corpus
}

func skillBM25Tokens(value string, limit int) []string {
	var out []string
	var word []rune
	var cjk []rune
	add := func(token string) {
		if limit > 0 && len(out) >= limit {
			return
		}
		if len([]rune(token)) < 2 || skillBM25StopWords[token] {
			return
		}
		out = append(out, token)
	}
	flushWord := func() {
		if len(word) > 0 {
			token := strings.ToLower(string(word))
			if len([]rune(token)) > 3 && strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
				token = strings.TrimSuffix(token, "s")
			}
			add(token)
		}
		word = word[:0]
	}
	flushCJK := func() {
		if len(cjk) > 1 {
			for i := 0; i+1 < len(cjk); i++ {
				add(string(cjk[i : i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, unicode.ToLower(r))
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func cjkSkillTerms(terms []string) []string {
	var out []string
	for _, term := range terms {
		runes := []rune(term)
		if len(runes) < 2 {
			continue
		}
		allHan := true
		for _, r := range runes {
			if !unicode.Is(unicode.Han, r) {
				allHan = false
				break
			}
		}
		if allHan {
			out = append(out, term)
		}
	}
	return out
}

func matchedSkillTerms(terms []string, document skillBM25Document) int {
	matched := 0
	for _, term := range terms {
		if document.nameTerms[term] > 0 || document.descriptionTerms[term] > 0 {
			matched++
		}
	}
	return matched
}

func skillTermFrequencies(tokens []string) map[string]int {
	frequencies := make(map[string]int, len(tokens))
	for _, token := range tokens {
		frequencies[token]++
	}
	return frequencies
}

func uniqueSkillTerms(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

func skillCanonicalNameMentioned(query, name string) bool {
	queryPhrase := normalizeSkillSearchPhrase(query)
	namePhrase := normalizeSkillSearchPhrase(name)
	if queryPhrase == "" || namePhrase == "" {
		return false
	}
	return strings.Contains(" "+queryPhrase+" ", " "+namePhrase+" ")
}

func normalizeSkillSearchPhrase(value string) string {
	var normalized strings.Builder
	spacePending := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if spacePending && normalized.Len() > 0 {
				normalized.WriteByte(' ')
			}
			normalized.WriteRune(r)
			spacePending = false
		} else {
			spacePending = true
		}
	}
	return strings.TrimSpace(normalized.String())
}
