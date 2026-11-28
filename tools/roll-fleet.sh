#!/bin/sh
# Fleet roll: wait for pi-ed to advance the roll-2026-11-27 tag, then restart the 15 agent
# deployments that install pi-ed as a package, and verify each pod actually landed on the new
# commit. pi-bot itself is deliberately NOT restarted here: it is this script's own pod, and it
# goes last, in a scheduled prompt, so a restart cannot kill the roll mid-flight.
#
# Why a moved tag is enough: every agent pod pins `git:github.com/initialed85/pi-ed@roll-2026-11-27`
# in /root/.pi/agent/settings.json, and a git package is reconciled to its ref at start
# (docs/packages.md: "A git package is fetched and reset to the remote branch or ref"). A tag is not
# a commit hash, so a force-advanced tag is followed on restart. That assumption is what step 3 checks.
set -u

NS=slopyphus
REPORT=/workspace/pi-bot/.pi/roll-report.txt
LOG=/workspace/pi-bot/.pi/roll.log
PIN=f3df9788   # the commit the fleet is on now; the roll only starts when the tag is past it

# ROLL_NOW=1 ROLL_FROM=<short sha the pods are actually running> skips the wait loop. Needed when
# the owner moved the tag BEFORE this script started (2026-11-28: pi-ed moved roll-2026-11-27 to
# 2d00c68 while the fleet sat on 009d3cb), because without it the script captures the live tag as
# OLD and then waits for a second move that will never come, aborting with no pod restarted.
# ROLL_FROM is the last-known fleet commit, so NEW != OLD holds and the roll proceeds.

# Peel the tag (`refs/tags/X^{}`). A pod checks out the COMMIT, so comparing against the annotated
# tag OBJECT sha labels every correctly-rolled pod a mismatch - which is exactly what the 04:47Z
# run reported: 15 pods at 2d00c68, all "MISMATCH ... want 8659dbb", where 8659dbb is the tag object
# and 2d00c68 the commit it points at. The roll was fine; the check was not.
REMOTE_TAG='refs/tags/roll-2026-11-27^{}'
OLD_FULL=$(git ls-remote https://github.com/initialed85/pi-ed "$REMOTE_TAG" 2>/dev/null | awk '{print $1}')
OLD=$(echo "$OLD_FULL" | cut -c1-7)
if [ "${ROLL_NOW:-0}" = 1 ] && [ -n "${ROLL_FROM:-}" ]; then OLD=$ROLL_FROM; OLD_FULL="$ROLL_FROM (ROLL_FROM)"; fi

log() { echo "$(date -u +%H:%M:%SZ) $*" | tee -a "$LOG"; }

: > "$REPORT"
log "roll-watch start; tag roll-2026-11-27 = $OLD, waiting for it to move off $PIN"

i=0
while [ "$i" -lt 60 ]; do
    NEW_FULL=$(git ls-remote https://github.com/initialed85/pi-ed "$REMOTE_TAG" 2>/dev/null | awk '{print $1}')
    NEW=$(echo "$NEW_FULL" | cut -c1-7)
    if [ -n "$NEW" ] && [ "$NEW" != "$OLD" ]; then
        log "tag moved: $OLD -> $NEW ($NEW_FULL)"
        break
    fi
    i=$((i + 1))
    sleep 45
done

NEW_FULL=$(git ls-remote https://github.com/initialed85/pi-ed "$REMOTE_TAG" 2>/dev/null | awk '{print $1}')
NEW=$(echo "$NEW_FULL" | cut -c1-7)
if [ -z "$NEW" ] || [ "$NEW" = "$OLD" ]; then
    log "ABORT: the tag never moved within 45 minutes; no pod was restarted"
    exit 1
fi

# The 16 agents are exactly the deployments that mount agent-credentials (15 here plus pi-bot).
DEPLOYS=$(kubectl get deploy -n "$NS" -o json | python3 -c '
import json,sys
d=json.load(sys.stdin)
for it in d["items"]:
    name=it["metadata"]["name"]
    if name=="pi-bot" or name=="pi-bot-bridge":
        continue
    vols=it["spec"]["template"]["spec"].get("volumes",[])
    if any(v.get("configMap",{}).get("name")=="agent-credentials" for v in vols):
        print(name)
')
log "targets: $(echo $DEPLOYS | tr '\n' ' ')"

ok=0; bad=0
for d in $DEPLOYS; do
    log "restart $d"
    kubectl rollout restart deployment/"$d" -n "$NS" >>"$LOG" 2>&1
    if ! kubectl rollout status deployment/"$d" -n "$NS" --timeout=240s >>"$LOG" 2>&1; then
        echo "$d ROLLOUT-TIMEOUT" >> "$REPORT"; bad=$((bad+1)); continue
    fi
    # Resolve the newest pod for this deployment from its own selector, not a guessed label.
    # matchLabels is `{"app":"x","service":"x"}`; kubectl -l needs `app=x,service=x`, so the
    # colons must be rewritten (an early version left them and selected nothing), and kubectl's
    # jsonpath has no negative index, so the newest pod comes from a sorted range plus tail.
    SEL=$(kubectl get deploy -n "$NS" "$d" -o jsonpath='{.spec.selector.matchLabels}' 2>/dev/null | tr -d '{}"' | sed 's/:/=/g')
    pod=$(kubectl get pods -n "$NS" -l "$SEL" --sort-by=.metadata.creationTimestamp -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | tail -1)
    got=$(kubectl exec -n "$NS" "$pod" -- git -C /root/.pi/agent/git/github.com/initialed85/pi-ed rev-parse --short=7 HEAD 2>/dev/null)
    if [ "$got" = "$NEW" ]; then
        echo "$d $pod $got ok" >> "$REPORT"; ok=$((ok+1)); log "  ok $pod at $got"
    else
        echo "$d $pod got=${got:-unknown} expected=$NEW" >> "$REPORT"; bad=$((bad+1)); log "  MISMATCH $pod got=${got:-unknown} want $NEW"
    fi
    sleep 15
done

log "done: $ok pods verified on $NEW ($NEW_FULL), $bad problems. pi-bot not rolled yet (deliberate, it is last)."
echo "target_commit $NEW_FULL" >> "$REPORT"
echo "pi-bot deferred, deliberate" >> "$REPORT"
