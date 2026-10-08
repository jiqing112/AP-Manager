#!/bin/bash
cd /root/ap-manage
zcode -c -p "$(cat CONTINUE.md)" 2>&1 | tee continue-run.log
