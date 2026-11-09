/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"

	ci "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// Kustomize maintains process-global OpenAPI state.
var kustomizeMu sync.Mutex

func localChartPath(p string) bool {
	return p != "" && !strings.Contains(p, ":") &&
		!strings.Contains(p, "\\") && !strings.HasPrefix(p, "/") &&
		path.Clean(p) != ".." && !strings.HasPrefix(path.Clean(p), "../")
}

func rejectKustomizeChildren(c ci.Charter) error {
	a, err := ci.NewAccessor(c)
	if err != nil {
		return err
	}
	for _, child := range a.Dependencies() {
		if v, ok := child.(*chart.Chart); ok && v.Metadata.Render != nil {
			return fmt.Errorf("Kustomize subcharts are not supported")
		}
		if err := rejectKustomizeChildren(child); err != nil {
			return err
		}
	}
	return nil
}

func renderKustomize(c ci.Charter, vals common.Values) (map[string]string, bool, error) {
	if err := rejectKustomizeChildren(c); err != nil {
		return nil, false, err
	}
	ch, ok := c.(*chart.Chart)
	if !ok || ch.Metadata.Render == nil {
		return nil, false, nil
	}
	result, err := buildKustomize(ch, vals)
	return result, true, err
}

func buildKustomize(ch *chart.Chart, vals common.Values) (map[string]string, error) {
	cfg := ch.Metadata.Render
	if cfg.Engine != "kustomize" {
		return nil, fmt.Errorf("unsupported render engine %q", cfg.Engine)
	}
	if ch.Metadata.Type == "library" || len(ch.Templates) != 0 ||
		len(ch.Dependencies()) != 0 || len(ch.Metadata.Dependencies) != 0 {
		return nil, fmt.Errorf("Kustomize requires an application chart without templates or dependencies")
	}
	values, err := vals.Table("Values")
	if err != nil {
		return nil, err
	}
	entry := cfg.Path
	if raw, exists := values["kustohelm"]; exists {
		config, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("kustohelm must be a mapping")
		}
		for key := range config {
			if key != "overlay" {
				return nil, fmt.Errorf("unsupported kustohelm field %q", key)
			}
		}
		if raw, exists := config["overlay"]; exists {
			name, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("kustohelm.overlay must be a string")
			}
			if name != "" {
				var found bool
				entry, found = cfg.Overlays[name]
				if !found {
					return nil, fmt.Errorf("unknown overlay %q", name)
				}
			}
		}
	}
	if !localChartPath(entry) {
		return nil, fmt.Errorf("invalid chart entry point %q", entry)
	}
	fs := filesys.MakeFsInMemory()
	for _, f := range ch.Files {
		if !localChartPath(f.Name) {
			return nil, fmt.Errorf("invalid chart file %q", f.Name)
		}
		// Check every Kustomization before allowing the loader to follow references.
		switch path.Base(f.Name) {
		case "kustomization.yaml", "kustomization.yml", "Kustomization":
			var config map[string]interface{}
			if err := yaml.Unmarshal(f.Data, &config); err != nil {
				return nil, err
			}
			for _, key := range []string{"resources", "bases", "components"} {
				if refs, ok := config[key].([]interface{}); ok {
					for _, ref := range refs {
						p, ok := ref.(string)
						if !ok || strings.Contains(p, ":") || strings.Contains(p, "?") ||
							strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
							return nil, fmt.Errorf("nonlocal %s reference in %s", key, f.Name)
						}
						resolved := path.Join(path.Dir(f.Name), p)
						if !localChartPath(resolved) {
							return nil, fmt.Errorf("reference escapes chart: %s", p)
						}
						// A local resource must exist in the chart filesystem.
						found := false
						for _, candidate := range ch.Files {
							if candidate.Name == resolved || strings.HasPrefix(candidate.Name, resolved+"/") {
								found = true
								break
							}
						}
						if !found {
							return nil, fmt.Errorf("missing chart resource %q", resolved)
						}
					}
				}
			}
			if _, ok := config["helmCharts"]; ok {
				return nil, fmt.Errorf("helmCharts is unsupported")
			}
		}
		filename := path.Join("/chart", f.Name)
		if err := fs.MkdirAll(path.Dir(filename)); err != nil {
			return nil, err
		}
		if err := fs.WriteFile(filename, f.Data); err != nil {
			return nil, err
		}
	}
	outer := map[string]interface{}{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{path.Join("chart", entry)},
	}
	if raw, exists := values["kustomize"]; exists {
		config, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("kustomize must be a mapping")
		}
		for key, value := range config {
			switch key {
			case "replicas", "images", "namePrefix", "nameSuffix", "labels", "commonAnnotations":
			case "patches":
				data, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				var patches []map[string]interface{}
				if err := json.Unmarshal(data, &patches); err != nil {
					return nil, err
				}
				for _, patch := range patches {
					if _, ok := patch["path"]; ok {
						return nil, fmt.Errorf("values patches must be inline")
					}
				}
			default:
				return nil, fmt.Errorf("unsupported kustomize field %q", key)
			}
			outer[key] = value
		}
	}
	data, err := yaml.Marshal(outer)
	if err != nil {
		return nil, err
	}
	if err := fs.WriteFile("/kustomization.yaml", data); err != nil {
		return nil, err
	}
	kustomizeMu.Lock()
	defer kustomizeMu.Unlock()
	resources, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, "/")
	if err != nil {
		return nil, fmt.Errorf("Kustomize build: %w", err)
	}
	output, err := resources.AsYaml()
	if err != nil {
		return nil, err
	}
	return map[string]string{ch.Name() + "/templates/kustomize.yaml": string(output)}, nil
}
