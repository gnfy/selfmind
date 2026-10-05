package envprofiles

import (
	"bufio"
	"io"
	"os"
	"strings"
)

func (c ApplyContext) resolveMountState(profiles []*EnvProfile) []*EnvProfile {
	result := make([]*EnvProfile, 0, len(profiles))
	for _, profile := range profiles {
		if profile == nil || len(profile.MountStateWhen) == 0 {
			result = append(result, profile)
			continue
		}
		required := false
		for _, condition := range profile.MountStateWhen {
			if c.mountConditionRequired(profile, condition) {
				required = true
				break
			}
		}
		if required {
			result = append(result, profile)
			continue
		}
		readonly := *profile
		readonly.MapRWAt = nil
		readonly.SynthesizeDir = nil
		result = append(result, &readonly)
	}
	return result
}

func (c ApplyContext) mountConditionRequired(profile *EnvProfile, condition MountStateCondition) bool {
	if c.EnvironmentOverrides["*"] || c.EnvironmentOverrides[condition.From.EnvVar] ||
		(condition.From.HomeRelPath != "" && c.EnvironmentOverrides["HOME"]) {
		return true
	}
	if condition.Selection != nil {
		for _, variable := range condition.Selection.EnvVars {
			if c.EnvironmentOverrides[variable] {
				return true
			}
		}
	}
	limit := condition.MaxBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	sections, all := c.configurationSections(profile, condition.Selection)
	keys := map[string]bool{}
	for _, key := range condition.Keys {
		keys[strings.ToLower(key)] = true
	}
	for _, path := range c.resolveSources(condition.From) {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.IsDir() || info.Size() > limit {
			return true
		}
		file, err := os.Open(path)
		if err != nil {
			return true
		}
		data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(data)) > limit {
			return true
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		section := ""
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				section = strings.TrimSpace(line[1 : len(line)-1])
				continue
			}
			key, _, valid := strings.Cut(line, "=")
			if !valid || section == "" {
				return true
			}
			if (all || sections[section]) && keys[strings.ToLower(strings.TrimSpace(key))] {
				return true
			}
		}
		if scanner.Err() != nil {
			return true
		}
	}
	return false
}

func (c ApplyContext) configurationSections(profile *EnvProfile, selection *ConfigurationSelection) (map[string]bool, bool) {
	if selection == nil || c.OpaquePrograms {
		return nil, true
	}
	sections := map[string]bool{}
	found := false
	for _, executable := range profile.MatchExecutables {
		for _, fields := range c.ProgramArguments[executable] {
			if len(fields) == 0 {
				return nil, true
			}
			found = true
			name := ""
			if c.Lookup != nil {
				for _, variable := range selection.EnvVars {
					if value, ok := c.Lookup(variable); ok && value != "" {
						name = value
						break
					}
				}
			}
			for i := 1; i < len(fields); i++ {
				if fields[i] == selection.Option {
					if i+1 >= len(fields) {
						return nil, true
					}
					i++
					name = fields[i]
				} else if value, ok := strings.CutPrefix(fields[i], selection.Option+"="); ok {
					name = value
				}
			}
			if strings.ContainsAny(name, "$`\\\n\r\t") {
				return nil, true
			}
			if name == "" || name == selection.DefaultSection {
				sections[selection.DefaultSection] = true
			} else {
				sections[selection.SectionPrefix+name] = true
			}
		}
	}
	return sections, !found
}
