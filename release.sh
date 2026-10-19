#!/bin/bash

now=$(date +"%Y%m%d%H%M%S")
docker buildx build --platform linux/amd64 -t charwin00/soruxgpt-saas-audit:v1.4 --push .
read -p "按回车键继续..."
echo "release success" $now