#!/bin/bash
# test_ws.sh — end-to-end WebSocket chat test
set -e

BASE="http://localhost:8080"
TS=$(date +%s)

echo "=== Register two users ==="
RESP_A=$(curl -s -X POST $BASE/auth/register -H "Content-Type: application/json" \
  -d "{\"pseudo\":\"user_a_$TS\",\"email\":\"a_$TS@test.com\",\"country\":\"FR\",\"password\":\"secret123\"}")
TOKEN_A=$(echo "$RESP_A" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "User A registered"

RESP_B=$(curl -s -X POST $BASE/auth/register -H "Content-Type: application/json" \
  -d "{\"pseudo\":\"user_b_$TS\",\"email\":\"b_$TS@test.com\",\"country\":\"DE\",\"password\":\"secret123\"}")
TOKEN_B=$(echo "$RESP_B" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "User B registered"

echo ""
echo "=== Match them ==="
curl -s -X POST $BASE/pool/join -H "Authorization: Bearer $TOKEN_A" > /dev/null
ROOM_ID=$(curl -s -X POST $BASE/pool/join -H "Authorization: Bearer $TOKEN_B" | \
  python3 -c "import sys,json; print(json.load(sys.stdin)['room_id'])")
echo "Matched in room: $ROOM_ID"

echo ""
echo "=== User A sends two messages via WebSocket ==="
printf 'Hello from A\nHow are you B?' | wscat -c "ws://localhost:8080/ws/$ROOM_ID?token=$TOKEN_A" --wait 1 &
PID_A=$!
sleep 2

echo ""
echo "=== User B sends one message via WebSocket ==="
printf 'Hey A, all good!' | wscat -c "ws://localhost:8080/ws/$ROOM_ID?token=$TOKEN_B" --wait 1 &
PID_B=$!
sleep 2

# Clean up wscat processes
kill $PID_A 2>/dev/null || true
kill $PID_B 2>/dev/null || true

echo ""
echo "=== Messages stored in DB ==="
curl -s $BASE/rooms/$ROOM_ID/messages -H "Authorization: Bearer $TOKEN_A" | python3 -m json.tool

echo ""
echo "=== Room info ==="
curl -s $BASE/rooms/$ROOM_ID -H "Authorization: Bearer $TOKEN_A" | python3 -m json.tool

echo ""
echo "DONE ✅"
