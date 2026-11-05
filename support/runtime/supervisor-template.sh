#!/bin/sh
# OFFLINE TEMPLATE. Install only after Lite 1 fingerprint/audio/ABI validation.
# Always invoke by the same absolute installed path; do not source this file.
set -f
umask 077
APP_ROOT=${ALISA_APP_ROOT:-/data/opt/alisa-chatgpt}
PROJECT_FILE=$APP_ROOT/project.id
LED_FEEDBACK_FILE=$APP_ROOT/led-feedback.enabled
ACTIVATION_MODE_FILE=$APP_ROOT/activation.mode
STATE=/tmp/alisa-chatgpt-supervisor
PIDFILE=$STATE/supervisor.pid
LOCK=$STATE/lock
CREDENTIAL=/data/yandex-station-chatgpt-voice/session.json
LIBS=/system/vendor/quasar/libs:/system/vendor/lib:/system/lib:/usr/lib:/lib
# Public settings only. Empty activation preserves Yandex; enable after device tests.
ACTIVATION_COMMAND=${ALISA_ACTIVATION_COMMAND:-}
# Uses the client's existing option; does not assert anything about account quota.
THINKING_EFFORT=${ALISA_THINKING_EFFORT:-disabled}
SELF=$0
child_pid= child_start= own_start= owns_lock=0 stopping=0
PROJECT_ID=
LED_FEEDBACK=0

event() { printf '%s\n' "$1"; }
diagnostic() { [ "${ALISA_DIAGNOSTICS:-0}" != 1 ] || event "$1" >&2; }
fail() { event "$1" >&2; exit 1; }
safe_absolute() {
    case "$1" in /*) ;; *) return 1 ;; esac
    case "$1" in /|*/|*//*|*/../*|*/./*|*/..|*/.|*[!a-zA-Z0-9_./-]*) return 1 ;; esac
}
private_path() {
    [ ! -L "$1" ] || return 1
    if [ "$2" = d ]; then [ -d "$1" ] || return 1; else [ -f "$1" ] || return 1; fi
    ls -ldn "$1" 2>/dev/null | awk -v kind="$2" '
        NR==1 { ok=substr($1,1,1)==kind && $3==0 && substr($1,5,6)=="------" }
        END { exit !ok }'
}
load_project() {
    PROJECT_ID=
    # An absent file preserves ordinary Voice. A present file must be private,
    # root-owned, regular and not a symlink; its contents are never shell code.
    if [ ! -e "$PROJECT_FILE" ] && [ ! -L "$PROJECT_FILE" ]; then return 0; fi
    private_path "$PROJECT_FILE" - && [ -r "$PROJECT_FILE" ] || return 1
    PROJECT_ID=$(LC_ALL=C awk '
        NR==1 && length($0)==36 && $0 ~ /^g-p-[0-9a-f]+$/ { id=$0; next }
        { bad=1 }
        END { if (NR!=1 || bad || id=="") exit 1; print id }
    ' "$PROJECT_FILE" 2>/dev/null) || { PROJECT_ID=; return 1; }
}
load_led_feedback() {
    LED_FEEDBACK=0
    if [ ! -e "$LED_FEEDBACK_FILE" ] && [ ! -L "$LED_FEEDBACK_FILE" ]; then return 0; fi
    private_path "$LED_FEEDBACK_FILE" - && [ -r "$LED_FEEDBACK_FILE" ] || return 1
    LED_FEEDBACK=$(LC_ALL=C awk '
        NR==1 && $0=="1" { enabled=1; next }
        { bad=1 }
        END { if (NR!=1 || bad || !enabled) exit 1; print 1 }
    ' "$LED_FEEDBACK_FILE" 2>/dev/null) || { LED_FEEDBACK=0; return 1; }
}
load_activation_mode() {
    # Missing configuration retains the existing startup environment. Read
    # only for start/run, so a bad selector never prevents stop or status.
    ACTIVATION_COMMAND=${ALISA_ACTIVATION_COMMAND:-}
    if [ ! -e "$ACTIVATION_MODE_FILE" ] && [ ! -L "$ACTIVATION_MODE_FILE" ]; then return 0; fi
    private_path "$APP_ROOT" d && private_path "$ACTIVATION_MODE_FILE" - && [ -r "$ACTIVATION_MODE_FILE" ] || return 1
    ls -ldn "$ACTIVATION_MODE_FILE" 2>/dev/null | awk '
        NR==1 { ok=$1=="-rw-------" && $3==0 }
        END { exit !ok }' || return 1
    activation_mode=$(LC_ALL=C awk '
        NR==1 && ($0=="native-yandex" || $0=="alice-command") { mode=$0; next }
        { bad=1 }
        END { if (NR!=1 || bad || mode=="") exit 1; print mode }
    ' "$ACTIVATION_MODE_FILE" 2>/dev/null) || return 1
    case "$activation_mode" in
        native-yandex) ACTIVATION_COMMAND= ;;
        alice-command)
            # Preserve the owner's existing phrase exactly. The client's
            # existing Unicode validator remains authoritative; reject empty,
            # oversized, multiline/control or ASCII punctuation-only input here.
            printf '%s\n' "$ACTIVATION_COMMAND" | LC_ALL=C awk '
                NR!=1 || length($0)>256 || $0 ~ /[[:cntrl:]]/ { bad=1 }
                { text=$0; gsub(/[[:space:][:punct:]]/, "", text); if (length(text)>0) usable=1 }
                END { exit !(NR==1 && !bad && usable) }' || return 1 ;;
        *) return 1 ;;
    esac
}
read_proc() {
    case "$1" in ''|*[!0-9]*) return 1 ;; esac
    proc_line=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
    proc_rest=${proc_line##*) }
    set -- $proc_rest
    [ "$#" -ge 20 ] || return 1
    PROC_STATE=$1
    PROC_PARENT=$2
    shift 19
    PROC_START=$1
    case "$PROC_START:$PROC_PARENT" in *[!0-9:]*|:*) return 1 ;; esac
    [ "$PROC_STATE" != Z ]
}
verify_supervisor() {
    [ "$1" -gt 1 ] 2>/dev/null || return 1
    read_proc "$1" && [ "$PROC_START" = "$2" ] || return 1
    proc_args=$(tr '\000' '\n' < "/proc/$1/cmdline" 2>/dev/null) || return 1
    case "
$proc_args
" in *"
$SELF
run
"*) return 0 ;; *) return 1 ;; esac
}
read_supervisor() {
    private_path "$STATE" d && private_path "$PIDFILE" - || return 1
    IFS=' ' read -r SUP_PID SUP_START SUP_EXTRA < "$PIDFILE" || return 1
    case "$SUP_PID:$SUP_START" in *[!0-9:]*|:*) return 1 ;; esac
    [ -z "$SUP_EXTRA" ] && verify_supervisor "$SUP_PID" "$SUP_START"
}
owned_child() {
    [ -n "$child_pid" ] || return 1
    read_proc "$child_pid" && [ "$PROC_PARENT" = "$$" ] || return 1
    if [ -n "$child_start" ]; then [ "$PROC_START" = "$child_start" ] || return 1; fi
    child_start=$PROC_START
}
stop_child() {
    if owned_child; then
        kill -TERM "$child_pid" 2>/dev/null || :
        grace=0
        while owned_child && [ "$grace" -lt 15 ]; do
            sleep 1
            grace=$((grace + 1))
        done
        if owned_child; then kill -KILL "$child_pid" 2>/dev/null || :; fi
    fi
    if [ -n "$child_pid" ]; then wait "$child_pid" 2>/dev/null || :; fi
    child_pid= child_start=
}
request_stop() {
    stopping=1
    trap '' HUP INT TERM
    stop_child
}
cleanup() {
    trap - 0
    trap '' HUP INT TERM
    stopping=1
    stop_child
    if [ "$owns_lock" = 1 ]; then
        if [ -f "$PIDFILE" ] && [ ! -L "$PIDFILE" ]; then
            IFS=' ' read -r cleanup_pid cleanup_start cleanup_extra < "$PIDFILE" || :
            if [ "$cleanup_pid:$cleanup_start" = "$$:$own_start" ] && [ -z "$cleanup_extra" ]; then
                rm -f "$PIDFILE"
            fi
        fi
        rmdir "$LOCK" 2>/dev/null || :
    fi
}
pause() {
    pause_ticks=$1
    while [ "$pause_ticks" -gt 0 ] && [ "$stopping" = 0 ]; do
        sleep 1
        pause_ticks=$((pause_ticks - 1))
    done
}
ready() {
    private_path "$APP_ROOT" d && private_path "$CLIENT" - && [ -x "$CLIENT" ] && [ -d /dev/snd ] || return 1
    private_path "${CREDENTIAL%/*}" d && private_path "$CREDENTIAL" - && [ -r "$CREDENTIAL" ] || return 1
    for required_path in /lib/ld-linux-armhf.so.3 /usr/bin/aplay; do
        [ -r "$required_path" ] || return 1
    done
    require_audio=0
    if [ -z "$ACTIVATION_COMMAND" ]; then
        require_audio=1
        for required_path in /system/vendor/quasar/libs/libiconv.so /system/vendor/quasar/libs/libtflite.so \
            /system/vendor/quasar/libs/libyandex_evil.so \
            /system/vendor/quasar/spotter_models/activation/yandex/flags.txt \
            /system/vendor/quasar/spotter_models/activation/yandex/acoustic_model.nnet \
            /system/vendor/quasar/spotter_models/activation/yandex/words.txt; do
            [ -r "$required_path" ] || return 1
        done
    fi
    # Only inspect LISTEN metadata; no microphone subscription or socket probe.
    # 9850=267A, 9875=2693; IPv4 loopback or wildcard can satisfy the client.
    awk -v require_audio="$require_audio" '$4=="0A" {
        if ($2=="0100007F:267A" || $2=="00000000:267A") audio=1
        if ($2=="0100007F:2693" || $2=="00000000:2693") alice=1
    } END { exit !(alice && (!require_audio || audio)) }' /proc/net/tcp 2>/dev/null
}
ensure_state() {
    if [ ! -e "$STATE" ] && [ ! -L "$STATE" ]; then mkdir "$STATE" 2>/dev/null || :; fi
    private_path "$STATE" d || fail ALISA_UNSAFE_RUNTIME
}
run_supervisor() {
    load_activation_mode || fail ALISA_INVALID_ACTIVATION_MODE
    ensure_state
    if ! mkdir "$LOCK" 2>/dev/null; then
        if read_supervisor; then event ALISA_ALREADY_RUNNING; return 0; fi
        fail ALISA_STALE_LOCK_INSPECT_REQUIRED
    fi
    owns_lock=1
    trap cleanup 0
    trap request_stop HUP INT TERM
    read_proc "$$" || fail ALISA_PROCESS_IDENTITY_FAILED
    own_start=$PROC_START
    [ ! -e "$PIDFILE" ] && [ ! -L "$PIDFILE" ] || fail ALISA_STALE_PID_INSPECT_REQUIRED
    (set -C; printf '%s %s\n' "$$" "$own_start" > "$PIDFILE") || fail ALISA_PID_PUBLICATION_FAILED
    backoff=1
    while [ "$stopping" = 0 ]; do
        if ! ready; then
            diagnostic ALISA_WAITING_NATIVE_RUNTIME
            pause 2
            continue
        fi
        if ! load_project; then
            diagnostic ALISA_INVALID_PROJECT_CONFIGURATION
            pause 2
            continue
        fi
        if ! load_led_feedback; then
            diagnostic ALISA_INVALID_LED_CONFIGURATION
            pause 2
            continue
        fi
        set -- "$CLIENT" run
        [ -z "${ALISA_VOICE:-}" ] || set -- "$@" "--voice=$ALISA_VOICE"
        [ -z "${ALISA_BACKEND_MODEL:-}" ] || set -- "$@" "--backend-model=$ALISA_BACKEND_MODEL"
        set -- "$@" "--backend-thinking-effort=$THINKING_EFFORT"
        [ -z "$ACTIVATION_COMMAND" ] || set -- "$@" "--activation-command=$ACTIVATION_COMMAND"
        [ -z "$PROJECT_ID" ] || set -- "$@" "--project-id=$PROJECT_ID"
        [ "$LED_FEEDBACK" = 0 ] || set -- "$@" --led-feedback
        LD_LIBRARY_PATH=$LIBS "$@" </dev/null >/dev/null 2>&1 &
        child_pid=$!
        child_start=
        owned_child || :
        if [ "$stopping" != 0 ]; then stop_child; break; fi
        diagnostic ALISA_CLIENT_STARTED
        wait "$child_pid" 2>/dev/null || :
        child_pid= child_start=
        [ "$stopping" = 0 ] || break
        diagnostic ALISA_CLIENT_EXITED_BACKOFF
        pause "$backoff"
        backoff=$((backoff * 2))
        [ "$backoff" -le 30 ] || backoff=30
    done
}

safe_absolute "$SELF" && safe_absolute "$APP_ROOT" || fail ALISA_ABSOLUTE_PATH_REQUIRED
case "$APP_ROOT" in /data/*) ;; *) fail ALISA_PERSISTENT_ROOT_REQUIRED ;; esac
CLIENT=$APP_ROOT/client
for required_tool in awk cat ls tr mkdir rmdir rm sleep kill id; do
    command -v "$required_tool" >/dev/null 2>&1 || fail ALISA_REQUIRED_TOOL_MISSING
done
[ "$(id -u)" = 0 ] || fail ALISA_ROOT_REQUIRED
[ "$#" -le 1 ] || fail ALISA_BAD_ARGUMENT
case "${1:-status}" in
    run) run_supervisor ;;
    start)
        load_activation_mode || fail ALISA_INVALID_ACTIVATION_MODE
        ensure_state
        if read_supervisor; then event ALISA_ALREADY_RUNNING; exit 0; fi
        [ ! -e "$LOCK" ] && [ ! -L "$LOCK" ] || fail ALISA_STALE_LOCK_INSPECT_REQUIRED
        [ -x "$SELF" ] || fail ALISA_SUPERVISOR_NOT_EXECUTABLE
        command -v setsid >/dev/null 2>&1 && command -v nohup >/dev/null 2>&1 || fail ALISA_DETACH_TOOL_MISSING
        setsid nohup "$SELF" run </dev/null >/dev/null 2>&1 &
        start_wait=0
        while [ "$start_wait" -lt 10 ]; do
            if read_supervisor; then event ALISA_SUPERVISOR_STARTED; exit 0; fi
            sleep 1
            start_wait=$((start_wait + 1))
        done
        fail ALISA_SUPERVISOR_START_FAILED ;;
    stop)
        if ! read_supervisor; then event ALISA_NOT_RUNNING; exit 0; fi
        stop_pid=$SUP_PID; stop_start=$SUP_START
        verify_supervisor "$stop_pid" "$stop_start" || fail ALISA_PROCESS_IDENTITY_CHANGED
        kill -TERM "$stop_pid" 2>/dev/null || fail ALISA_STOP_SIGNAL_FAILED
        stop_wait=0
        while verify_supervisor "$stop_pid" "$stop_start" && [ "$stop_wait" -lt 20 ]; do
            sleep 1
            stop_wait=$((stop_wait + 1))
        done
        if verify_supervisor "$stop_pid" "$stop_start"; then fail ALISA_STOP_TIMEOUT; fi
        event ALISA_STOPPED ;;
    status)
        if read_supervisor; then event ALISA_SUPERVISOR_RUNNING; else event ALISA_NOT_RUNNING; exit 3; fi ;;
    *) fail ALISA_BAD_ARGUMENT ;;
esac
