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

package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
)

func TestKustomizeChart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  map[string]interface{}
		want    string
		failure string
	}{
		{"default", nil, "replicas: 1", ""},
		{"overlay", map[string]interface{}{"kustohelm": map[string]interface{}{"overlay": "production"}}, "replicas: 3", ""},
		{"override", map[string]interface{}{"kustohelm": map[string]interface{}{"overlay": "production"}, "kustomize": map[string]interface{}{"replicas": []interface{}{map[string]interface{}{"name": "home", "count": 7}}}}, "replicas: 7", ""},
		{"unknown", map[string]interface{}{"kustohelm": map[string]interface{}{"overlay": "missing"}}, "", "unknown overlay"},
		{"unsupported", map[string]interface{}{"kustomize": map[string]interface{}{"resources": []string{"https://example.org"}}}, "", "unsupported kustomize field"},
		{"external patch", map[string]interface{}{"kustomize": map[string]interface{}{"patches": []interface{}{map[string]interface{}{"path": "/tmp/patch"}}}}, "", "must be inline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := loader.Load("../../../../examples/home")
			require.NoError(t, err)
			for _, packaged := range []bool{false, true} {
				if packaged {
					archive, err := chartutil.Save(ch, t.TempDir())
					require.NoError(t, err)
					ch, err = loader.Load(archive)
					require.NoError(t, err)
					require.NotNil(t, ch.Metadata.Render)
				}
				vals, err := util.ToRenderValuesWithSchemaValidation(ch, tc.values, common.ReleaseOptions{Name: "test", Namespace: "default"}, nil, true)
				require.NoError(t, err)
				output, err := new(engine.Engine).RenderWithContext(context.Background(), ch, vals)
				if tc.failure != "" {
					require.ErrorContains(t, err, tc.failure)
					continue
				}
				require.NoError(t, err)
				require.True(t, strings.Contains(output["home/templates/kustomize.yaml"], tc.want), output)
			}
		})
	}
}

func TestKustomizeRejectsUnsafeCharts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*chart.Chart)
		failure string
	}{
		{"templates", func(c *chart.Chart) { c.Templates = []*common.File{{Name: "templates/test.yaml"}} }, "without templates"},
		{"dependencies", func(c *chart.Chart) { c.AddDependency(&chart.Chart{Metadata: &chart.Metadata{Name: "child"}}) }, "without templates"},
		{"escape", func(c *chart.Chart) { c.Metadata.Render.Path = "../outside" }, "invalid chart entry"},
		{"remote", func(c *chart.Chart) {
			c.Files = append(c.Files, &common.File{Name: "base/kustomization.yaml", Data: []byte("resources:\n- https://example.org/resource.yaml\n")})
		}, "nonlocal"},
		{"missing", func(c *chart.Chart) { c.Metadata.Render.Path = "missing" }, "Kustomize build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loader.Load("../../../../examples/home")
			require.NoError(t, err)
			tc.mutate(c)
			_, err = new(engine.Engine).RenderWithContext(context.Background(), c, common.Values{"Values": map[string]interface{}{}})
			require.ErrorContains(t, err, tc.failure)
		})
	}
}
