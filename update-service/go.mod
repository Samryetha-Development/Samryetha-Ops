module samryetha-status

// go 指令提升到 go1.26.10：最低要求（floor），go 指令支持 patch 版本。
// go directive raised to go1.26.10: minimum requirement (floor); patch versions are supported.
// 保留下方 toolchain 行是兼容性考虑：GOTOOLCHAIN=auto（默认）且本机 Go 较旧时按它自动下载 go1.26.10。
// Keeping the toolchain line below is for compatibility: with GOTOOLCHAIN=auto (default) and an older local Go, it auto-downloads go1.26.10.
// 但 toolchain 只是"建议"：本机有更新版（如 1.27.2，仍在 CVE-2026-111943 范围）时 auto 照用更新版，约束不住。
// But toolchain is only a suggestion: with a newer local Go (e.g. 1.27.2, still in CVE-2026-111943), auto uses the newer one.
// 真正硬约束是构建入口显式 export GOTOOLCHAIN=go1.26.10（精确版本、禁止升级到更高小版本）。
// The real hard constraint is the explicit export GOTOOLCHAIN=go1.26.10 at build entry points (exact version, no upgrade).
go 1.26.10

toolchain go1.26.10
