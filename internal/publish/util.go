package publish

import "encoding/json"

// jsonMarshalIndent 把 meta.json 写成人类可读格式。
//
// 刻意用缩进而不是紧凑格式：meta.json 是排障时最常被 `cat` 的文件
// （"这台节点当时跑的是哪一版、期望监听哪些端口"），可读性比几个字节重要。
func jsonMarshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
