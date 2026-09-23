#!/bin/bash
# 状态页生成器入口。由 cron 每分钟调用（见 README）。
export PATH="/home/ubuntu/.local/bin:/home/ubuntu/.nvm/versions/node/v24.20.0/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
exec python3 "$(dirname "$0")/generate.py"
