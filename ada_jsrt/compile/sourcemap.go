package compile

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SourceMap 是 Artifact.SourceMap 里单个文件的最小 sourcemap 视图。
// 只实现 stack trace 需要的映射：generated line/column → sources line/column。
type SourceMap struct {
	Sources []string
	Names   []string
	lines   [][]mapSegment
}

type mapSegment struct {
	genCol   int
	srcIdx   int
	origLine int
	origCol  int
}

type rawSourceMap struct {
	Version  int      `json:"version"`
	Sources  []string `json:"sources"`
	Names    []string `json:"names"`
	Mappings string   `json:"mappings"`
}

const base64VLQ = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// ParseSourceMap 解析 esbuild 产出的 sourcemap。
func ParseSourceMap(data []byte) (*SourceMap, error) {
	var raw rawSourceMap
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("compile: sourcemap 解析失败: %w", err)
	}
	sm := &SourceMap{Sources: raw.Sources, Names: raw.Names}
	srcIdx, origLine, origCol, nameIdx := 0, 0, 0, 0
	for _, line := range strings.Split(raw.Mappings, ";") {
		genCol := 0
		segments := []mapSegment{}
		for _, seg := range strings.Split(line, ",") {
			if seg == "" {
				continue
			}
			values, err := decodeVLQ(seg)
			if err != nil {
				return nil, err
			}
			if len(values) == 0 {
				continue
			}
			genCol += values[0]
			if len(values) >= 4 {
				srcIdx += values[1]
				origLine += values[2]
				origCol += values[3]
				if len(values) >= 5 {
					nameIdx += values[4]
					_ = nameIdx
				}
				segments = append(segments, mapSegment{
					genCol: genCol, srcIdx: srcIdx, origLine: origLine, origCol: origCol,
				})
			}
		}
		sm.lines = append(sm.lines, segments)
	}
	return sm, nil
}

// Lookup 把生成的 1-based line / 1-based column 映射回原始位置。
func (sm *SourceMap) Lookup(line, column int) (source string, origLine, origCol int, ok bool) {
	if sm == nil || line < 1 || line > len(sm.lines) {
		return "", 0, 0, false
	}
	genCol := column - 1
	if genCol < 0 {
		genCol = 0
	}
	segments := sm.lines[line-1]
	var found *mapSegment
	for i := range segments {
		if segments[i].genCol <= genCol {
			found = &segments[i]
		} else {
			break
		}
	}
	if found == nil || found.srcIdx < 0 || found.srcIdx >= len(sm.Sources) {
		return "", 0, 0, false
	}
	return sm.Sources[found.srcIdx], found.origLine + 1, found.origCol + 1, true
}

func decodeVLQ(s string) ([]int, error) {
	var values []int
	value, shift := 0, 0
	for i := 0; i < len(s); i++ {
		digit := strings.IndexByte(base64VLQ, s[i])
		if digit < 0 {
			return nil, fmt.Errorf("compile: sourcemap VLQ 字符非法: %q", s[i])
		}
		cont := digit & 32
		value += (digit & 31) << shift
		if cont != 0 {
			shift += 5
			continue
		}
		negative := value&1 != 0
		value >>= 1
		if negative {
			value = -value
		}
		values = append(values, value)
		value, shift = 0, 0
	}
	return values, nil
}
