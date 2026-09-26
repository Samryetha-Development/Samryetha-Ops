// cfgcheck 校验 deploy.yaml 是否可被解析器正确读取。
//
// 为什么单独做成命令：deploy.yaml 是人手写的，而解析器是受限子集实现。
// 两者不匹配时（写了不支持的语法、路径指错），必须在**提交前**发现，
// 而不是等部署时才发现（前面已经因为配置错误浪费过多轮排查）。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"samryetha/services/deploycfg"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	path := filepath.Join(root, "etc", "deploy.yaml")
	if _, err := os.Stat(path); err != nil {
		// 允许在无配置的目录里跳过（例如 CI 只跑代码检查）
		fmt.Printf("skip: %s not found\n", path)
		return
	}
	f, err := deploycfg.Load(path)
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}

	// 逐项检查"配置里引用的东西是否自洽"
	problems := []string{}
	for _, t := range f.Targets {
		if t.ID == "" {
			problems = append(problems, "target 缺少 id")
			continue
		}
		if t.WorkDir == "" {
			problems = append(problems, fmt.Sprintf("target %s 缺少 workdir", t.ID))
		}
		if t.Source == "" {
			problems = append(problems, fmt.Sprintf("target %s 缺少 source", t.ID))
		}
		if len(t.Health) == 0 {
			problems = append(problems, fmt.Sprintf("target %s 没有健康检查（健康门是回滚的依据）", t.ID))
		}
		// 构建命令里的路径必须与 workdir 自洽（这是之前踩过的坑：
		// dev 的构建命令指向了 main 的目录，静默地构建了错误的代码）
		for i, cmd := range t.Build {
			if t.WorkDir != "" && len(cmd) > 0 {
				// 粗查：命令里出现的绝对路径是否以 workdir 开头
				// （只检查形如 cd /abs/path 的片段，避免误判）
				if p := extractCD(cmd); p != "" && !isUnder(p, t.WorkDir) {
					problems = append(problems, fmt.Sprintf(
						"target %s build[%d] 进入 %s，但 workdir 是 %s（可能构建到别的目标）",
						t.ID, i+1, p, t.WorkDir))
				}
			}
		}
	}

	if len(problems) > 0 {
		fmt.Println("FAIL: 配置自洽性检查未通过")
		for _, p := range problems {
			fmt.Println("  -", p)
		}
		os.Exit(1)
	}
	fmt.Printf("OK: %d 个目标，配置自洽\n", len(f.Targets))
}

// extractCD 从形如 "cd /path && ..." 的命令里取出路径（取不到返回空）。
func extractCD(cmd string) string {
	const prefix = "cd "
	if len(cmd) < len(prefix) || cmd[:len(prefix)] != prefix {
		return ""
	}
	rest := cmd[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' || rest[i] == '&' {
			return rest[:i]
		}
	}
	return rest
}

// isUnder 判断 path 是否在 base 之下（按路径分量比较，避免同前缀误判）。
func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (len(rel) > 0 && rel[0] != '.')
}
