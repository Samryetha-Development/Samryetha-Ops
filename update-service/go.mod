module samryetha-status

// go 指令为 go1.26.10：最低要求（floor），go 指令支持 patch 版本。
// go directive set to go1.26.10: minimum requirement (floor); the go directive supports patch versions.
// 不保留 toolchain 行：go 指令已是 patch 版本 go1.26.10，此时 toolchain go1.26.10 与之相同、属冗余，
// go mod tidy 会删除它，并让 go vet/build 报 "updates to go.mod needed"。而且 toolchain 行本就只是
// "建议"——GOTOOLCHAIN 默认 auto 时本机有更新版（如 1.27.2，仍在 CVE-2026-111943 范围）会照用，约束不住。
// 真正硬约束是构建入口显式 export GOTOOLCHAIN=go1.26.10（精确版本、禁止升级到更高小版本）。
// The toolchain line is dropped: with the go directive already at patch go1.26.10, a toolchain go1.26.10
// line would be redundant, so go mod tidy removes it and go vet/build then report "updates to go.mod needed".
// The toolchain line is also only a suggestion — GOTOOLCHAIN defaults to auto, which uses a newer local Go
// (e.g. 1.27.2, still within CVE-2026-111943) if present. The real hard constraint is the explicit export
// GOTOOLCHAIN=go1.26.10 at build entry points (exact version, no upgrade to a higher patch).
go 1.26.10
