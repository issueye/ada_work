package ui

import "testing"

// TestRowsToEnv 守住编辑行的合并规则:空行/空值忽略、非法名与重名报错。
func TestRowsToEnv(t *testing.T) {
	rows := []botEnvRow{
		{Key: " ADA_LLM_MODEL ", Value: "m1"},
		{Key: "", Value: ""},
		{Key: "ADA_LLM_MODEL", Value: "m2"},
	}
	if _, err := rowsToEnv(rows); err == "" {
		t.Fatalf("重名应报错")
	}

	if _, err := rowsToEnv([]botEnvRow{{Key: "bad-name", Value: "x"}}); err == "" {
		t.Fatalf("非法名应报错")
	}

	out, err := rowsToEnv([]botEnvRow{
		{Key: "ADA_LLM_MODEL", Value: "m1"},
		{Key: "EMPTY", Value: "   "},
		{Key: "", Value: ""},
	})
	if err != "" {
		t.Fatalf("不应报错: %s", err)
	}
	if len(out) != 1 || out["ADA_LLM_MODEL"] != "m1" {
		t.Fatalf("空值应被忽略,只保留有效项: %v", out)
	}

	out, err = rowsToEnv(nil)
	if err != "" || out != nil {
		t.Fatalf("空表应返回 nil 且无错误: %v %q", out, err)
	}
}

// TestEnvRowsOfSorted 编辑行按键名排序,避免每帧顺序抖动。
func TestEnvRowsOfSorted(t *testing.T) {
	rows := envRowsOf(map[string]string{"B": "2", "A": "1"})
	if len(rows) != 2 || rows[0].Key != "A" || rows[1].Key != "B" {
		t.Fatalf("行顺序不对: %+v", rows)
	}
	if envRowsOf(nil) != nil {
		t.Fatalf("没有变量时应返回 nil")
	}
}
