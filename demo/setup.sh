#!/bin/sh
# Builds the directory the demo recording runs in.
set -eu
dir=${1:-/tmp/incant-demo}
rm -rf "$dir"
mkdir -p "$dir/logs" "$dir/node_modules/left-pad"
i=1
while [ $i -le 40 ]; do
  printf 'GET /page/%s 200\n' "$i" > "$dir/logs/app-$i.log"
  i=$((i + 1))
done
# Thirty of the forty logs are a month old.
touch -t 202601010000 "$dir"/logs/app-[1-9].log "$dir"/logs/app-1[0-9].log "$dir"/logs/app-2[0-9].log "$dir"/logs/app-30.log
echo 'module.exports = 1' > "$dir/node_modules/left-pad/index.js"
printf '3\n1\n2\n' > "$dir/scores.txt"
echo "$dir"
