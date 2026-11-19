package library

import (
	"path"
	"sort"

	"github.com/AndreasByrsting/live2motion/internal/store"
)

// PairCandidate 一组可配对的 HEIC + 视频。
type PairCandidate struct {
	ContentID string
	Heic      store.Asset
	Video     store.Asset
}

// PairStats 配对统计。
type PairStats struct {
	Groups      int // 有 ContentIdentifier 的分组数
	OrphanHeic  int // 有 CID 但找不到匹配视频
	OrphanVideo int // 有 CID 但找不到匹配 HEIC
	Ambiguous   int // 同 CID 多文件，按规则择一后剩余
	CrossFolder int // 跨目录配对数量（同目录优先，跨目录仍允许）
}

// BuildPairs 按 ContentIdentifier 把资产配成 Live Photo。
//
// 规则（配对只看 UUID，绝不看文件名）：
//  1. 同一 ContentIdentifier 下的 HEIC 与视频才可能配对；
//  2. 优先配对同一目录内的文件（多数图库同目录存放）；
//  3. 剩余文件按路径排序一对一贪心匹配，保证结果确定可复现；
//  4. 多出来的记为孤儿，不生成任务，只统计告警。
func BuildPairs(assets []store.Asset) ([]PairCandidate, PairStats) {
	var stats PairStats

	type group struct {
		heics  []store.Asset
		videos []store.Asset
	}
	groups := make(map[string]*group, len(assets)/2+1)
	for _, a := range assets {
		if a.ContentID == "" {
			continue
		}
		g, ok := groups[a.ContentID]
		if !ok {
			g = &group{}
			groups[a.ContentID] = g
		}
		if a.Kind == store.KindHEIC {
			g.heics = append(g.heics, a)
		} else {
			g.videos = append(g.videos, a)
		}
	}

	cids := make([]string, 0, len(groups))
	for cid := range groups {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	stats.Groups = len(cids)

	pairs := make([]PairCandidate, 0, len(cids))
	for _, cid := range cids {
		g := groups[cid]
		sort.Slice(g.heics, func(i, j int) bool { return g.heics[i].RelPath < g.heics[j].RelPath })
		sort.Slice(g.videos, func(i, j int) bool { return g.videos[i].RelPath < g.videos[j].RelPath })

		if len(g.heics) == 0 {
			stats.OrphanVideo += len(g.videos)
			continue
		}
		if len(g.videos) == 0 {
			stats.OrphanHeic += len(g.heics)
			continue
		}

		usedH := make([]bool, len(g.heics))
		usedV := make([]bool, len(g.videos))

		// 第一轮：同目录精确匹配
		byDir := make(map[string][]int, len(g.videos))
		for i, v := range g.videos {
			d := path.Dir(v.RelPath)
			byDir[d] = append(byDir[d], i)
		}
		for i, h := range g.heics {
			d := path.Dir(h.RelPath)
			idx, ok := byDir[d]
			if !ok || len(idx) == 0 {
				continue
			}
			vi := idx[0]
			byDir[d] = idx[1:]
			usedH[i], usedV[vi] = true, true
			pairs = append(pairs, PairCandidate{ContentID: cid, Heic: h, Video: g.videos[vi]})
		}

		// 第二轮：剩余按顺序一对一
		var restH, restV []int
		for i := range g.heics {
			if !usedH[i] {
				restH = append(restH, i)
			}
		}
		for i := range g.videos {
			if !usedV[i] {
				restV = append(restV, i)
			}
		}
		n := len(restH)
		if len(restV) < n {
			n = len(restV)
		}
		for k := 0; k < n; k++ {
			h := g.heics[restH[k]]
			v := g.videos[restV[k]]
			if path.Dir(h.RelPath) != path.Dir(v.RelPath) {
				stats.CrossFolder++
			}
			usedH[restH[k]], usedV[restV[k]] = true, true
			pairs = append(pairs, PairCandidate{ContentID: cid, Heic: h, Video: v})
		}

		// 第三轮：统计多余
		extraH, extraV := 0, 0
		for i := range g.heics {
			if !usedH[i] {
				extraH++
			}
		}
		for i := range g.videos {
			if !usedV[i] {
				extraV++
			}
		}
		stats.OrphanHeic += extraH
		stats.OrphanVideo += extraV
		stats.Ambiguous += extraH + extraV
	}
	return pairs, stats
}
