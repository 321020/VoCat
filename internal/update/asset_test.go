package update

import (
	"reflect"
	"testing"
)

func TestAssetNamesFor(t *testing.T) {
	tests := []struct {
		goos   string
		goarch string
		want   []string
	}{
		{"linux", "amd64", []string{"vocat-linux-amd64"}},
		{"linux", "386", []string{"vocat-linux-386"}},
		{"linux", "arm64", []string{"vocat-linux-arm64", "vocat-linux-aarch64"}},
		{"linux", "arm", []string{"vocat-linux-armv7", "vocat-linux-arm"}},
	}
	for _, item := range tests {
		if got := assetNamesFor(item.goos, item.goarch); !reflect.DeepEqual(got, item.want) {
			t.Errorf("assetNamesFor(%q, %q) = %#v, want %#v", item.goos, item.goarch, got, item.want)
		}
	}
}
