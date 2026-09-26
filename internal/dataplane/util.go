package dataplane

import (
	"sort"

	"github.com/shengyu/edgelink/internal/model"
)

// sortedListeners 把 map 形式的监听集合转成稳定排序的切片。
// 稳定排序是必须的：期望监听列表会进 meta.json 与 config_versions，
// 顺序不稳定会导致 content_hash 每次都变，版本号无意义地增长。
func sortedListeners(set map[string]model.Listener) []model.Listener {
	out := make([]model.Listener, 0, len(set))
	for _, l := range set {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}
