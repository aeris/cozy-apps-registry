#!/bin/bash

_term() {
  kill -INT $pid
}

trap _term SIGINT

couch_addr="http://127.0.0.1:5984"
curl -s $couch_addr/_all_dbs | jq -r '.[]' | grep "registry-" | \
  xargs -I % python -c "import urllib; print urllib.quote('''%''', safe='')" | \
  xargs -I % curl -X DELETE ${couch_addr}/%

reg1=("bank" "drive" "health" "photos" "collect")
reg2=("drive" "homebook" "bank" "collect")
reg3=("bank" "collect" "drive" "onboarding" "photos" "settings")

cozy-apps-registry add-editor cozy
cozy-apps-registry --port 8081 --contexts reg1,reg2,reg3 serve &
pid=$!
sleep 1

for name in "${reg1[@]}"; do
  curl \
    --silent --fail \
    -X POST http://localhost:8081/registry/reg1/ \
    -H 'Content-Type:application/json' \
    -H "Authorization: Token $(cozy-apps-registry gen-token cozy)" \
    -d "{\"slug\": \"${name}\", \"editor\":\"cozy\", \"type\": \"webapp\"}" \
    > /dev/null
done

for name in "${reg2[@]}"; do
  curl \
    --silent --fail \
    -X POST http://localhost:8081/registry/reg2/ \
    -H 'Content-Type:application/json' \
    -H "Authorization: Token $(cozy-apps-registry gen-token cozy)" \
    -d "{\"slug\": \"${name}\", \"editor\":\"cozy\", \"type\": \"webapp\"}" \
    > /dev/null
done

for name in "${reg3[@]}"; do
  curl \
    --silent --fail \
    -X POST http://localhost:8081/registry/reg3/ \
    -H 'Content-Type:application/json' \
    -H "Authorization: Token $(cozy-apps-registry gen-token cozy)" \
    -d "{\"slug\": \"${name}\", \"editor\":\"cozy\", \"type\": \"webapp\"}" \
    > /dev/null
done

echo "Ready !"
cat
