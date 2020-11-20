/*
Copyright 2020 The Knative Authors

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

// Package packages provides abstract tools for managing a packaged release.
package packages

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Asset provides an abstract interface for describing a resource which should be stored on the disk.
type Asset struct {
	Name      string
	URL       string
	secondary bool
}

// Release provides an interface for a release which contains multiple assets at the same release (TagName)
type Release struct {
	Org     string
	Repo    string
	TagName string
	Created time.Time
	Assets  assetList //[]Asset
}

// Less provides a method for implementing `sort.Slice` to ensure that assets
// are applied in the correct order.
func (a Asset) Less(b Asset) bool {
	// HACK for pre-install jobs, which are deprecated, because the job needs to
	// *complete*, not just be applied, before the next manifests can be
	// applied.
	if strings.HasSuffix(a.Name, "-pre-install-jobs.yaml") {
		return true
	}
	if strings.HasSuffix(b.Name, "-pre-install-jobs.yaml") {
		return false
	}

	if strings.HasSuffix(a.Name, "-crds.yaml") {
		return true
	}
	if strings.HasSuffix(b.Name, "-crds.yaml") {
		return false
	}
	if strings.HasSuffix(a.Name, "-post-install-jobs.yaml") {
		return false
	}
	if strings.HasSuffix(b.Name, "-post-install-jobs.yaml") {
		return true
	}

	// HACK for eventing, which lists the sugar controller after the
	// channel/broker despite collating before.
	if strings.HasSuffix(a.Name, "-sugar-controller.yaml") {
		return false
	}
	if strings.HasSuffix(b.Name, "-sugar-controller.yaml") {
		return true
	}
	if a.secondary != b.secondary {
		// Sort primary assets before secondary assets
		return b.secondary
	}
	return a.Name < b.Name
}

// Less provides a sort on Releases by TagName.
func (r Release) Less(b Release) bool {
	return semver.Compare(r.TagName, b.TagName) > 0
}

// String implements `fmt.Stringer`.
func (r Release) String() string {
	return fmt.Sprintf("%s/%s %s", r.Org, r.Repo, r.TagName)
}

// assetList provides an interface for operating on a set of assets.
type assetList []Asset

// Len is part of `sort.Interface`.
func (al assetList) Len() int {
	return len(al)
}

// Less is part of `sort.Interface`.
func (al assetList) Less(i, j int) bool {
	return al[i].Less(al[j])
}

// Swap is part of `sort.Interface`.
func (al assetList) Swap(i, j int) {
	al[i], al[j] = al[j], al[i]
}

type releaseList []Release

// Len is part of `sort.Interface`.
func (rl releaseList) Len() int {
	return len(rl)
}

// Less is part of `sort.Interface`.
func (rl releaseList) Less(i, j int) bool {
	return rl[i].Less(rl[j])
}

// Swap is part of `sort.Interface`.
func (rl releaseList) Swap(i, j int) {
	rl[i], rl[j] = rl[j], rl[i]
}

// FilterAssets retains only assets where `accept` returns a non-empty string.
// `accept` may return a *different* string in the case of assets which should
// be renamed.
func (al assetList) FilterAssets(accept func(string) string) assetList {
	retval := make([]Asset, 0, len(al))
	for _, asset := range al {
		if name := accept(asset.Name); name != "" {
			asset.Name = name
			retval = append(retval, asset)
		}
	}

	return retval
}

// HandleRelease processes the files for a given release of the specified
// Package.
func HandleRelease(ctx context.Context, client *http.Client, p Package, r Release, allReleases map[string][]Release) error {
	majorMinor := semver.MajorMinor(r.TagName)
	shortName := strings.TrimPrefix(r.TagName, "v")
	path := filepath.Join("cmd", "operator", "kodata", p.Name, shortName)
	err := os.MkdirAll(path, 0755)
	if err != nil {
		return err
	}

	// TODO: make a copy of r's assets to avoid modifying the global cache.
	assets := make(assetList, 0, len(r.Assets))
	assets = append(assets, r.Assets.FilterAssets(p.Primary.Accept(r.TagName))...)
	for _, src := range p.Additional {
		candidates := allReleases[src.String()]
		sort.Sort(releaseList(candidates))
		start, end := -1, len(candidates)
		for i, srcRelease := range candidates {
			// Collect matching minor versions
			comp := semver.Compare(majorMinor, semver.MajorMinor(srcRelease.TagName))
			if start == -1 && comp == 0 {
				start = i
			}
			if comp > 0 {
				end = i
				break
			}
		}
		candidates = candidates[start:end]
		timeMatch := len(candidates) - 1
		for i, srcRelease := range candidates {
			// TODO: more sophisticated alignment options, for example, always use latest matching minor.
			if r.Created.After(srcRelease.Created) {
				timeMatch = i
				break
			}
		}

		candidate := candidates[timeMatch]
		newAssets := candidate.Assets.FilterAssets(src.Accept(candidate.TagName))
		for i := range newAssets {
			newAssets[i].secondary = true
		}
		assets = append(assets, newAssets...)
		log.Printf("Using %s/%s with %s/%s", candidate.String(), candidate.TagName, r.String(), r.TagName)
	}
	sort.Sort(assets)

	// Download assets and store them.
	for i, asset := range assets {
		fileName := fmt.Sprintf("%d-%s", i+1, asset.Name)
		file, err := os.OpenFile(filepath.Join(path, fileName), os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("Unable to open %s: %w", fileName, err)
		}
		defer file.Close()
		log.Print(asset.URL)
		fetch, err := client.Get(asset.URL)
		if err != nil {
			return fmt.Errorf("Unable to fetch %s: %w", fileName, err)
		}
		defer fetch.Body.Close()
		_, err = io.Copy(file, fetch.Body)
		if err != nil {
			return fmt.Errorf("Unable to write to %s: %w", fileName, err)
		}
	}
	return nil
}

// LastN selects the last N minor releases (including all patch releases) for a
// given sequence of releases, which need not be sorted.
func LastN(minors int, allReleases []Release) []Release {
	retval := make(releaseList, len(allReleases))

	copy(retval, allReleases)
	sort.Sort(retval)

	previous := semver.MajorMinor(retval[0].TagName)
	for i, r := range retval {
		if semver.MajorMinor(r.TagName) == previous {
			continue // Only count/act if the minor release changes
		}
		previous = semver.MajorMinor(r.TagName)
		minors--
		if minors == 0 {
			retval = retval[:i]
			break
		}
	}

	return retval
}