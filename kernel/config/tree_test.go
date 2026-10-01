package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// "恢复默认"必须真的能把覆盖值删掉，并回落到下一层来源。
//
// 背景：只有 Set 没有 Unset 时，一旦保存过就再也回不到"跟随默认值"——
// 覆盖值会永久压过描述文件，而界面上没有任何撤销入口。
func TestUnsetRemovesOverrideAndPrunesEmptyParents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var"), 0o700); err != nil {
		t.Fatal(err)
	}
	tree, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	// 描述文件给的默认值先合并进来（不会被覆盖值顶掉的那一层）
	tree.MergeDefaults(map[string]any{
		"svc": map[string]any{"interval": "15m", "mode": "auto"},
	})
	if err := tree.Set(root, "svc.interval", "1m"); err != nil {
		t.Fatal(err)
	}
	if v, _ := tree.Get("svc.interval"); v != "1m" {
		t.Fatalf("覆盖值未生效：%#v", v)
	}

	if err := tree.Unset(root, "svc.interval"); err != nil {
		t.Fatal(err)
	}
	// 注意语义：配置树里**没有分层回退**——MergeDefaults 是把默认值并进同一棵树，
	// 所以 Unset 之后这个键在本进程内就是"不存在"。真正的"回落到默认值"由调用方
	// （装配层 resolveSchedule）用描述文件里的值兜底；本测试断言的是"覆盖值确实没了"。
	if _, ok := tree.Get("svc.interval"); ok {
		t.Fatal("覆盖值应已从树中删除")
	}

	// 重启语义：重新加载 + 再次合并默认值，键应带着**默认值**回来。
	tree2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	tree2.MergeDefaults(map[string]any{
		"svc": map[string]any{"interval": "15m", "mode": "auto"},
	})
	if v, ok := tree2.Get("svc.interval"); !ok || v != "15m" {
		t.Fatalf("重启后应由描述文件重新提供默认值 15m，实际 ok=%v v=%#v", ok, v)
	}

	// 落盘的 json 里不该残留空的父映射
	b, err := os.ReadFile(filepath.Join(root, "var", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	svc, _ := disk["svc"].(map[string]any)
	if svc == nil {
		t.Fatalf("svc 下还有 mode 默认值，父级不该被清掉：%s", string(b))
	}
	if _, still := svc["interval"]; still {
		t.Fatalf("覆盖值应已从磁盘删除，实际 %s", string(b))
	}
	if _, hasMode := svc["mode"]; !hasMode {
		t.Fatalf("不该误删其它键：%s", string(b))
	}
}

// 删掉最后一个键时，父映射应被清理掉，不留空 {}。
func TestUnsetPrunesNowEmptyParent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var"), 0o700); err != nil {
		t.Fatal(err)
	}
	tree, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Set(root, "only.child", "x"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Unset(root, "only.child"); err != nil {
		t.Fatal(err)
	}
	if _, ok := tree.Get("only.child"); ok {
		t.Fatal("键应已删除")
	}
	b, _ := os.ReadFile(filepath.Join(root, "var", "config.json"))
	var disk map[string]any
	_ = json.Unmarshal(b, &disk)
	if _, ok := disk["only"]; ok {
		t.Fatalf("空的父映射应被清掉，实际 %s", string(b))
	}
}

func TestUnsetRejectsEmptyPath(t *testing.T) {
	tree, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Unset(t.TempDir(), "  "); err == nil {
		t.Fatal("空路径应报错，而不是删掉整棵树或静默成功")
	}
}
