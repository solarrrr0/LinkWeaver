package main

import "testing"

func TestGitHubReleaseSelection(t *testing.T) {
	for _, x := range []struct {
		a, b string
		want bool
	}{{"0.3.10", "0.3.9", true}, {"0.3.3", "0.3.3", false}, {"0.2.9", "0.3.3", false}, {"1.0.0", "0.99.9", true}, {"bad", "0.3.3", false}} {
		if versionNewer(x.a, x.b) != x.want {
			t.Fatalf("version comparison %s %s", x.a, x.b)
		}
	}
	name := "LinkWeaver-Update-0.3.4-Windows-x64.zip"
	r := githubRelease{Tag: "v0.3.4", Assets: []githubAsset{{Name: name, Size: 100, URL: githubProjectURL + "/releases/download/v0.3.4/" + name}, {Name: name + ".sha256", URL: githubProjectURL + "/releases/download/v0.3.4/" + name + ".sha256"}}}
	s, err := selectRelease(r)
	if err != nil || !s.Available || s.Latest != "0.3.4" {
		t.Fatal("valid release not selected", err)
	}
	r.Assets[0].URL = "https://example.com/payload.zip"
	if _, err = selectRelease(r); err == nil {
		t.Fatal("external executable source accepted")
	}
	for _, raw := range []string{"http://github.com/" + githubRepo + "/releases/download/v1/a", "https://github.com.evil.test/" + githubRepo + "/releases/download/v1/a", "https://github.com/other/repo/releases/download/v1/a"} {
		if releaseDownloadURL(raw) {
			t.Fatal("untrusted release URL accepted")
		}
	}
}
