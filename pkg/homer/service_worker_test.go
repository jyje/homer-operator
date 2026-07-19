/*
Copyright 2024 RajSingh.

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

package homer

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func findContainer(t *testing.T, d appsv1.Deployment, name string) *corev1.Container {
	t.Helper()
	for i, c := range d.Spec.Template.Spec.Containers {
		if c.Name == name {
			return &d.Spec.Template.Spec.Containers[i]
		}
	}
	t.Fatalf("container %q not found", name)
	return nil
}

func hasVolumeMount(c *corev1.Container, name, path string) bool {
	for _, vm := range c.VolumeMounts {
		if vm.Name == name && vm.MountPath == path {
			return true
		}
	}
	return false
}

func TestGenerateOfflineHTML(t *testing.T) {
	html := GenerateOfflineHTML("My Dashboard", "/assets/logo.png", "#123456", "#abcdef")

	if !strings.Contains(html, "My Dashboard") {
		t.Errorf("expected offline HTML to contain the title, got: %s", html)
	}
	if !strings.Contains(html, "/assets/logo.png") {
		t.Errorf("expected offline HTML to contain the logo URL")
	}
	if !strings.Contains(html, "#123456") || !strings.Contains(html, "#abcdef") {
		t.Errorf("expected offline HTML to contain the theme colors")
	}
}

func TestGenerateOfflineHTMLDefaults(t *testing.T) {
	html := GenerateOfflineHTML("", "", "", "")

	if !strings.Contains(html, "Dashboard") {
		t.Errorf("expected default title fallback, got: %s", html)
	}
	if !strings.Contains(html, "#3367d6") {
		t.Errorf("expected default theme color fallback")
	}
	if !strings.Contains(html, "#ffffff") {
		t.Errorf("expected default background color fallback")
	}
}

func TestGenerateServiceWorkerJS(t *testing.T) {
	sw := GenerateServiceWorkerJS("<html>offline</html>", []string{"/assets/logo.png", "/assets/font.woff2"})

	if !strings.Contains(sw, "'/offline.html'") {
		t.Errorf("expected sw.js to precache the offline page")
	}
	if !strings.Contains(sw, "'/assets/logo.png'") || !strings.Contains(sw, "'/assets/font.woff2'") {
		t.Errorf("expected sw.js to precache CacheAssets entries")
	}
	if !strings.Contains(sw, "event.request.mode !== 'navigate'") {
		t.Errorf("expected sw.js to only intercept navigation requests")
	}
}

func TestGenerateServiceWorkerJSCacheNameChangesWithContent(t *testing.T) {
	swA := GenerateServiceWorkerJS("<html>A</html>", nil)
	swB := GenerateServiceWorkerJS("<html>B</html>", nil)

	if swA == swB {
		t.Fatalf("expected different offline content to produce different sw.js output")
	}

	extractCacheName := func(s string) string {
		idx := strings.Index(s, "homer-offline-")
		if idx == -1 {
			return ""
		}
		end := strings.Index(s[idx:], "'")
		return s[idx : idx+end]
	}

	nameA := extractCacheName(swA)
	nameB := extractCacheName(swB)
	if nameA == "" || nameB == "" {
		t.Fatalf("expected to find a cache name in generated sw.js")
	}
	if nameA == nameB {
		t.Errorf("expected cache name to change when offline HTML content changes, got same name %q", nameA)
	}
}

func TestCreateDeploymentServiceWorkerDisabledByDefault(t *testing.T) {
	d := CreateDeployment("test", "default", nil, nil, &DeploymentConfig{})

	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "www-volume" {
			t.Fatalf("did not expect www-volume when ServiceWorker is disabled")
		}
	}
	if len(d.Spec.Template.Spec.InitContainers) != 0 {
		t.Fatalf("did not expect init containers when ServiceWorker is disabled")
	}

	homerContainer := findContainer(t, d, "test")
	if !hasVolumeMount(homerContainer, "assets-volume", "/www/assets") {
		t.Errorf("expected homer container to mount assets-volume at /www/assets when ServiceWorker is disabled")
	}
}

func TestCreateDeploymentServiceWorkerEnabled(t *testing.T) {
	cfg := &DeploymentConfig{
		HomerImage:           "b4bz/homer:latest",
		ServiceWorkerEnabled: true,
		ServiceWorkerJS:      "// sw",
		OfflineHTML:          "<html>offline</html>",
	}
	d := CreateDeployment("test", "default", nil, nil, cfg)

	foundVolume := false
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "www-volume" {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Fatalf("expected www-volume when ServiceWorker is enabled")
	}

	if len(d.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected one init container when ServiceWorker is enabled, got %d", len(d.Spec.Template.Spec.InitContainers))
	}
	initContainer := d.Spec.Template.Spec.InitContainers[0]
	if !hasVolumeMount(&initContainer, "www-volume", "/shared-www") {
		t.Errorf("expected init container to mount www-volume at /shared-www")
	}

	homerContainer := findContainer(t, d, "test")
	if !hasVolumeMount(homerContainer, "www-volume", "/www") {
		t.Errorf("expected homer container to mount www-volume at /www when ServiceWorker is enabled")
	}

	syncContainer := findContainer(t, d, "config-sync")
	if !hasVolumeMount(syncContainer, "www-volume", "/www") {
		t.Errorf("expected config-sync container to mount www-volume at /www when ServiceWorker is enabled")
	}

	cmd := strings.Join(syncContainer.Command, " ")
	if !strings.Contains(cmd, "/www/sw.js") || !strings.Contains(cmd, "/www/offline.html") {
		t.Errorf("expected config-sync command to write sw.js and offline.html")
	}
	if !strings.Contains(cmd, "serviceWorker.register") {
		t.Errorf("expected config-sync command to patch index.html with a Service Worker registration script")
	}
}
