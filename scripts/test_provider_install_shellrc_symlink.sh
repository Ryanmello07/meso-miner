#!/bin/bash
# shellcheck disable=SC1090,SC2016,SC2034,SC2329
# The block under test is lifted out of the installer and eval'd in this scope,
# so it sources $lib, reads every variable assigned below and calls user_shell();
# ShellCheck cannot follow any of that. SC2016 fires on grep patterns that are
# meant to match literal text in the lifted block.
#
# Prove the FreeBSD installer's PATH hint never writes through a name the
# service user controls, that the caller's identity decides how the edit runs
# (su only as root: a non-root caller is never prompted, and a caller who is
# neither root nor the service user is warned and skipped), and that the append
# itself is honest. That user owns the directory the rc file lives in, so:
#   - a planted symlink is refused untouched;
#   - the whole edit (mktemp, copy, test, append, mode, rename) runs AS that
#     user when root hands it over, so a name swapped after the check cannot
#     make root follow a link;
#   - an installer already running as the service user edits the rc directly,
#     with no su call at all (su would prompt for a password);
#   - a commented-out PATH line does not suppress the append, a missing final
#     newline does not glue the entry onto the last line, and a file that ends
#     with one does not gain a blank line;
#   - a failed edit leaves the rc untouched instead of renaming a half-written
#     temp over it.
#
# The block under test is inline inside do_install(), so this lifts the exact
# bytes out of the installer with awk and evals them with a stubbed install
# context. The main dispatch is stripped and the rest sourced, so pr_warn and
# pr_info are the real shipped helpers. Nothing here needs a FreeBSD host.
set -u

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/Provider_Install_FreeBSD.sh"
fails=0
skipped=0

pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }
skip() { printf 'skip %s\n' "$1"; skipped=$((skipped + 1)); }

assert_eq() { # expected actual message
    if [ "$1" = "$2" ]; then pass "$3"; else
        fail "$3"
        printf '     expected: %s\n     actual:   %s\n' "$1" "$2"
    fi
}

# The string handed to `su -c`. FreeBSD's `su -m` runs the CALLER's login
# shell, which can be csh: `ulimit` does not exist there (csh spells it `limit`)
# and `2>` is a syntax error. The limit and the command therefore both travel
# inside a POSIX shell, so the only thing a csh caller parses is `/bin/sh -c`
# plus one single-quoted word. `exec` keeps the script's exit status and stdin.
su_payload() { # <write-limit 0|1> <command>
    if [ "$1" = 1 ]; then
        printf "/bin/sh -c 'ulimit -f 0 2>/dev/null; exec %s'" "$2"
    else
        printf "/bin/sh -c '%s'" "$2"
    fi
}

# The block under test hands the edit to su so it runs as the service user.
# Re-run as root when we can, so the ownership assertions are a genuine
# privilege switch rather than a no-op.
if [ "$(id -u)" != 0 ] && [ "${SHELLRC_TEST_NO_SUDO:-0}" != 1 ] && sudo -n true 2>/dev/null; then
    exec sudo -n env SHELLRC_TEST_NO_SUDO=1 TMPDIR="${TMPDIR:-/tmp}" \
        "$here/$(basename "$0")" "$@"
fi

tmpd="$(mktemp -d "${TMPDIR:-/tmp}/shellrc-test.XXXXXX")" || exit 1
trap 'rm -rf "$tmpd"' EXIT
# The block under test writes as the service user, so that user has to be able
# to traverse into the sandboxes below.
chmod 755 "$tmpd"

svc="$(id -un)"
if [ "$(id -u)" = 0 ]; then
    # A real unprivileged account, so the switch is a genuine ownership change.
    # SUDO_USER is the account that re-exec'd this suite here: the system
    # accounts are nologin and often expired, and switching to them fails
    # before the block under test ever runs.
    for candidate in "${SUDO_USER:-}" nobody nfsnobody daemon; do
        case "$candidate" in
            ''|root|"$(id -un)") continue ;;
        esac
        if id "$candidate" > /dev/null 2>&1; then svc="$candidate"; break; fi
    done
fi
printf '== %s (uid %s, service user %s)\n' "$(basename "$here")" "$(id -u)" "$svc"

owner_of() { stat -c '%U' "$1" 2>/dev/null || stat -f '%Su' "$1" 2>/dev/null; }
mode_of()  { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null; }

# The block under test writes as the service user, so the sandbox has to look
# like the real layout: a directory and an rc file owned by that user.
own_svc() { # path
    if [ "$(id -u)" = 0 ] && [ "$svc" != root ]; then chown "$svc" "$1" 2>/dev/null; fi
}
new_box() { # dir
    mkdir -p "$1" && own_svc "$1"
}

# run_variant <installer> <sandbox> <expect> [write-limit] [identity]
# expect is "guard" for the fixed file (the edit runs as the service user, with
# the symlink guard) or "old" for the pinned pre-fix version (root performs the
# write and a plain grep decides whether the entry is already there).
# write-limit=1 squeezes RLIMIT_FSIZE so the PATH append itself fails.
# identity simulates the caller `id` reports: empty is the real one, "svc" is
# the service user, "other" is a third account. Only the branch choice is
# simulated; su is stubbed to fail loudly in both, because reaching it there
# would mean a password prompt.
run_variant() {
    local inst="$1" box="$2" expect="$3" limit="${4:-0}" ident="${5:-}"
    local lib="$2/.lib.sh" blk="$2/.block.sh"
    sed '/^case "$operation" in/,$d' "$inst" > "$lib"
    # From the shell_rc assignment to the `rm -rf "$tmpdir"` that follows the
    # UPDATING block. The lift stops short of the 4-space `fi` that closes that
    # block (its opener is outside the lift, so including it would leave the
    # fragment unbalanced), along with the blank line before the rm. Ending on
    # the rm instead of on a bare `fi` keeps this working while the block
    # contains its own nested `fi` at 4-space indent, as the heredoc does.
    if ! awk '
        index($0, "_shell=\"$(user_shell") > 0 { on = 1 }
        on && $0 == "    rm -rf \"$tmpdir\"" { exit }
        on { n++; line[n] = $0 }
        END {
            if (n < 3 || line[n-1] != "    fi") {
                print "EXTRACT-BROKEN: unexpected block tail" > "/dev/stderr"
                exit 1
            }
            for (i = 1; i <= n - 2; i++) print line[i]
        }
    ' "$inst" > "$blk"; then
        return 1
    fi

    # A truncated or unbalanced lift must fail here, not eval into a syntax
    # error after doing half the work.
    if ! sh -n "$blk" 2>/dev/null; then
        echo "EXTRACT-BROKEN: lifted block does not parse" >&2
        return 1
    fi

    # Fail loudly if the extraction ever stops covering the code under test,
    # instead of silently passing on an empty block.
    if [ "$expect" = guard ]; then
        if ! grep -qF '[ -L "$shell_rc" ]' "$blk"; then
            echo "EXTRACT-MISSING: symlink guard not in lifted block" >&2
            return 1
        fi
        if ! grep -qF 'su -m "$service_user" -c ' "$blk"; then
            echo "EXTRACT-MISSING: service-user handover not in lifted block" >&2
            return 1
        fi
        if ! grep -qF '/bin/sh -s' "$blk"; then
            echo "EXTRACT-MISSING: direct /bin/sh handover not in lifted block" >&2
            return 1
        fi
    fi
    if [ "$expect" = old ] && ! grep -qF 'grep -qsF "$install_path/bin" "$shell_rc"' "$blk"; then
        echo "EXTRACT-MISSING: pre-fix grep test not in lifted block" >&2
        return 1
    fi

    (
        . "$lib"
        # The stripped tail ends with `[ -n "$operation" ] && shift`, which
        # returns 1 when sourced with no args; `:` keeps that from looking
        # like a failed source.
        :
        user_shell() { printf '%s\n' "${SHELLRC_TEST_USER_SHELL:-/bin/sh}"; }
        # The installer chooses how to run the edit from the caller it sees, so
        # the non-root branches are exercised by stubbing `id` rather than by
        # being unprivileged (the suite re-execs under sudo when it can). uid
        # 1000 is a plain user; whether the lookup of the service account
        # matches decides direct versus skip.
        if [ -n "$ident" ]; then
            id() {
                case "${1:-}" in
                    -u) if [ "$#" -gt 1 ]; then
                            if [ "$ident" = svc ]; then echo 1000; else echo 2000; fi
                        else
                            echo 1000
                        fi ;;
                    -un) if [ "$ident" = svc ]; then echo "$svc"; else echo urnet-shellrc-other; fi ;;
                    *)   command id "$@" ;;
                esac
            }
        fi
        # The block hands the whole edit to `su`, which cannot authenticate in
        # a test. This stub keeps the real invocation's shape — `-m`, login
        # name, then `-c CMD`, with the script on stdin — and still switches
        # user for real when the suite runs as root against an unprivileged
        # service account, so the ownership assertions stay honest. Note the
        # login name comes BEFORE -c, which is what FreeBSD's su needs (there
        # -c before the login name would be a login class).
        su() {
            _su_user=""; _su_cmd=""
            while [ $# -gt 0 ]; do
                case "$1" in
                    -m|-l|-f) shift ;;
                    -c) _su_cmd="$2"; shift 2 ;;
                    *) _su_user="$1"; shift ;;
                esac
            done
            # RLIMIT_FSIZE=0: mktemp still creates an empty file, every write
            # fails, so the append-failure path can be exercised. su resets
            # resource limits from the target's login class, so when a real
            # switch happens the limit has to ride inside the command.
            _su_payload="$(su_payload "${SHELLRC_TEST_WRITE_LIMIT:-0}" "$_su_cmd")"
            if [ -n "$_su_user" ] && [ "$_su_user" != "$(id -un)" ] && [ "$(id -u)" = 0 ]; then
                # The test accounts have nologin shells, which util-linux
                # refuses to run. FreeBSD's -s is a MAC label rather than a
                # shell, and its -m already runs the caller's shell, so the
                # override is only wanted (and only correct) elsewhere.
                case "$(uname -s)" in
                    FreeBSD) command su -m "$_su_user" -c "$_su_payload" ;;
                    *)       command su -m -s /bin/sh "$_su_user" -c "$_su_payload" ;;
                esac
            else
                if [ "${SHELLRC_TEST_WRITE_LIMIT:-0}" = 1 ]; then ulimit -f 0 2>/dev/null; fi
                sh -c "$_su_cmd"
            fi
        }
        if [ -n "$ident" ]; then
            # Reaching su in these modes would be the defect under test: it
            # prompts a caller who cannot switch. Fail loudly if that happens.
            su() { printf 'SU-CALLED: %s\n' "$*"; return 97; }
        fi
        install_path="$box/install"
        service_user="$svc"
        service_home="$box"
        UPDATING=0
        SHELLRC_TEST_WRITE_LIMIT="$limit"
        export SHELLRC_TEST_WRITE_LIMIT
        eval "$(cat "$blk")"
    ) 2>&1
}

printf '\n-- a planted symlink is refused\n'
box="$tmpd/symlink"; new_box "$box"
printf 'SENTINEL: must not change\n' > "$box/secret"
cp "$box/secret" "$tmpd/secret.before"
ln -s "$box/secret" "$box/.profile"
out="$(run_variant "$installer" "$box" guard)"; rc=$?

if cmp -s "$box/secret" "$tmpd/secret.before"; then
    pass "link target is byte-for-byte unchanged"
else
    fail "link target changed: $(tr -d '\n' < "$box/secret")"
fi
if grep -qF "$box/install/bin" "$box/secret" 2>/dev/null; then
    fail "PATH line was written through the symlink"
else
    pass "no PATH line in the link target"
fi
if [ -L "$box/.profile" ]; then pass "the symlink was left in place"; else fail "the symlink was replaced"; fi
case "$out" in
    *symlink*) pass "the refusal is reported, not silently skipped" ;;
    *) fail "no symlink warning in output: $out" ;;
esac
case "$out" in
    *"$box/.profile"*) pass "the warning names the offending path" ;;
    *) fail "warning does not name the path: $out" ;;
esac
assert_eq 0 "$rc" "a symlinked rc does not fail the install"
if [ -z "$(ls "$box"/.profile.new.* 2>/dev/null)" ]; then
    pass "no temp file left behind"
else
    fail "temp file left behind: $(ls "$box"/.profile.new.*)"
fi

printf '\n-- a normal rc gets the PATH line exactly once\n'
box="$tmpd/normal"; new_box "$box"
printf 'export KEEP=1\n' > "$box/.profile"
own_svc "$box/.profile"
chmod 640 "$box/.profile"
run_variant "$installer" "$box" guard > /dev/null
run_variant "$installer" "$box" guard > /dev/null

assert_eq 1 "$(grep -cF "$box/install/bin" "$box/.profile")" "one PATH line after two runs"
assert_eq 'export KEEP=1' "$(head -1 "$box/.profile")" "existing content is preserved"
assert_eq 640 "$(mode_of "$box/.profile")" "the rc file keeps its mode"
if [ -f "$box/.profile" ] && [ ! -L "$box/.profile" ]; then
    pass "the result is a regular file"
else
    fail "the result is not a regular file"
fi
assert_eq "$svc" "$(owner_of "$box/.profile")" "the rc file belongs to the service user"
if [ -z "$(ls "$box"/.profile.new.* 2>/dev/null)" ]; then
    pass "no temp file left behind"
else
    fail "temp file left behind: $(ls "$box"/.profile.new.*)"
fi

printf '\n-- a missing rc is created\n'
box="$tmpd/missing"; new_box "$box"
run_variant "$installer" "$box" guard > /dev/null
if [ -f "$box/.profile" ] && [ ! -L "$box/.profile" ]; then
    pass "the rc file was created as a regular file"
else
    fail "the rc file was not created"
fi
assert_eq 1 "$(grep -cF "$box/install/bin" "$box/.profile")" "the created file has one PATH line"

printf '\n-- a commented-out PATH line does not suppress the append\n'
box="$tmpd/commented"; new_box "$box"
printf '# export PATH="%s/install/bin:$PATH"\nexport KEEP=1\n' "$box" > "$box/.profile"
own_svc "$box/.profile"
run_variant "$installer" "$box" guard > /dev/null
commented="# export PATH=\"$box/install/bin:\$PATH\""
assert_eq "$commented" "$(head -1 "$box/.profile")" "the commented line is left alone"
assert_eq 2 "$(grep -cF "$box/install/bin" "$box/.profile")" "an active entry is appended beside the comment"
assert_eq "export PATH=\"$box/install/bin:\$PATH\"" "$(tail -1 "$box/.profile")" "the appended line is an active export"
run_variant "$installer" "$box" guard > /dev/null
assert_eq 2 "$(grep -cF "$box/install/bin" "$box/.profile")" "the active entry is not appended twice"

printf '\n-- an rc without a trailing newline keeps its last line\n'
box="$tmpd/nonewline"; new_box "$box"
printf 'export KEEP=1' > "$box/.profile"
own_svc "$box/.profile"
run_variant "$installer" "$box" guard > /dev/null
assert_eq 'export KEEP=1' "$(head -1 "$box/.profile")" "the last line is not glued to the PATH entry"
assert_eq 2 "$(wc -l < "$box/.profile" | tr -d ' ')" "the entry got a line of its own"
assert_eq "export PATH=\"$box/install/bin:\$PATH\"" "$(tail -1 "$box/.profile")" "the PATH line is intact"

printf '\n-- an rc ending in a newline gains no blank line\n'
box="$tmpd/newline"; new_box "$box"
printf 'export KEEP=1\n' > "$box/.profile"
own_svc "$box/.profile"
run_variant "$installer" "$box" guard > /dev/null
assert_eq 2 "$(wc -l < "$box/.profile" | tr -d ' ')" "still exactly two lines"
assert_eq 'export KEEP=1' "$(sed -n 1p "$box/.profile")" "line 1 is unchanged"
assert_eq "export PATH=\"$box/install/bin:\$PATH\"" "$(sed -n 2p "$box/.profile")" "line 2 is the PATH entry"

printf '\n-- a failed edit leaves the rc untouched\n'
box="$tmpd/writefail"; new_box "$box"
: > "$box/.profile"
own_svc "$box/.profile"
if [ "$(id -u)" = 0 ]; then
    # Root hands the edit to su, and the stub squeezes RLIMIT_FSIZE into the
    # target's shell: the copy lands and the append itself fails.
    chmod 640 "$box/.profile"
    failed_step="the append"
    out="$(run_variant "$installer" "$box" guard 1)"
else
    # Direct mode has no su stub to carry a limit through, so the copy is made
    # to fail instead: the rc is unreadable. The temp is still created and
    # still has to be removed, and the rc must not change.
    chmod 000 "$box/.profile"
    failed_step="the copy"
    out="$(run_variant "$installer" "$box" guard)"
fi
want_mode="$(mode_of "$box/.profile")"
if [ -s "$box/.profile" ] || [ -L "$box/.profile" ]; then
    fail "the rc was replaced: $(ls -l "$box/.profile")"
else
    pass "the rc is unchanged (no half-written entry moved over it)"
fi
assert_eq "$want_mode" "$(mode_of "$box/.profile")" "the rc keeps its mode"
case "$out" in
    *"Could not update"*) pass "the failure is reported, not silently dropped" ;;
    *) fail "no failure warning in output: $out" ;;
esac
if [ -z "$(ls "$box"/.profile.new.* 2>/dev/null)" ]; then
    pass "the temp was removed after $failed_step failed"
else
    fail "temp left behind: $(ls "$box"/.profile.new.*)"
fi

printf '\n-- an installer already running as the service user never calls su\n'
box="$tmpd/direct"; new_box "$box"
printf 'export KEEP=1\n' > "$box/.profile"
own_svc "$box/.profile"
out="$(run_variant "$installer" "$box" guard 0 svc)"
assert_eq 1 "$(grep -cF "$box/install/bin" "$box/.profile")" "the direct branch appends the PATH line"
case "$out" in
    *SU-CALLED*) fail "su was invoked although the caller is the service user: $out" ;;
    *)           pass "su was not invoked (no password prompt)" ;;
esac
case "$out" in
    *"Could not update"*) fail "the direct edit reported a failure: $out" ;;
    *)                    pass "no failure reported" ;;
esac

printf '\n-- a caller who is neither root nor the service user is warned, not prompted\n'
box="$tmpd/other"; new_box "$box"
printf 'export KEEP=1\n' > "$box/.profile"
own_svc "$box/.profile"
before="$(cat "$box/.profile")"
out="$(run_variant "$installer" "$box" guard 0 other)"; rc=$?
assert_eq "$before" "$(cat "$box/.profile")" "the rc is left alone"
case "$out" in
    *SU-CALLED*) fail "su was invoked from a caller who cannot switch without a password: $out" ;;
    *)           pass "su was not invoked (no password prompt)" ;;
esac
case "$out" in
    *"Not editing $box/.profile"*) pass "the warning names the rc file" ;;
    *)                            fail "the warning does not name the rc file: $out" ;;
esac
assert_eq 0 "$rc" "skipping the PATH hint does not fail the install"

printf '\n-- a tcsh login shell with a space in its home gets a quoted setenv\n'
box="$tmpd/tcsh space"; new_box "$box"
SHELLRC_TEST_USER_SHELL=/bin/tcsh
export SHELLRC_TEST_USER_SHELL
run_variant "$installer" "$box" guard > /dev/null
run_variant "$installer" "$box" guard > /dev/null
unset SHELLRC_TEST_USER_SHELL
if [ -f "$box/.cshrc" ]; then
    pass "the tcsh rc was created"
else
    fail "no .cshrc for a tcsh login shell"
fi
assert_eq "setenv PATH \"$box/install/bin:\$PATH\"" "$(tail -1 "$box/.cshrc" 2>/dev/null)" \
    "the value is one double-quoted word, spaces included"
assert_eq 1 "$(grep -cF "$box/install/bin" "$box/.cshrc" 2>/dev/null)" \
    "the quoted entry is recognised as present, not appended twice"
if command -v tcsh > /dev/null 2>&1; then
    readback="$(SHELLRC_RC="$box/.cshrc" tcsh -f -c 'source "$SHELLRC_RC" >& /dev/null; echo "$PATH"' 2>&1)"
    case "$readback" in
        "$box/install/bin:"*) pass "tcsh reads the line back with the whole path" ;;
        *)                    fail "tcsh read back [$readback]" ;;
    esac
else
    skip "tcsh is not installed here; the quoting is asserted textually"
fi
if [ -e "$box/.profile" ]; then fail "a tcsh user also got a POSIX .profile"; else pass "no POSIX rc was created"; fi

printf '\n-- a /bin/csh login shell is handled like tcsh\n'
box="$tmpd/csh"; new_box "$box"
SHELLRC_TEST_USER_SHELL=/bin/csh
export SHELLRC_TEST_USER_SHELL
run_variant "$installer" "$box" guard > /dev/null
unset SHELLRC_TEST_USER_SHELL
assert_eq "setenv PATH \"$box/install/bin:\$PATH\"" "$(tail -1 "$box/.cshrc" 2>/dev/null)" \
    "csh gets the csh rc and setenv syntax"
if [ -e "$box/.profile" ]; then fail "a csh user got a POSIX .profile"; else pass "no POSIX rc was created"; fi

printf '\n-- the su payload is POSIX-sh quoted, so a csh caller never parses it\n'
payload="$(su_payload 1 '/bin/sh -s')"
assert_eq "/bin/sh -c 'ulimit -f 0 2>/dev/null; exec /bin/sh -s'" "$payload" \
    "the write limit and the command ride in one POSIX shell"
case "$payload" in
    "/bin/sh -c '"*"'" ) pass "a csh caller parses only /bin/sh -c and one quoted word" ;;
    *) fail "payload is not a single quoted word for csh: $payload" ;;
esac
assert_eq '0STDIN-OK' \
    "$(printf 'ulimit -f\necho STDIN-OK\n' | sh -c "$payload" 2>/dev/null | tr -d '[:space:]')" \
    "the wrapped shell keeps the limit and still reads stdin"

# The pre-fix installer is pinned by COMMIT, not read from origin/main: once
# this fix merges, origin/main IS the fixed version, so a falsification run
# against it would pass no matter what the fix broke. This SHA is the parent of
# the merge that FIRST added the symlink guard (PR #46): it still writes through
# a planted link, which is the bug this suite is named for, and it stays
# reachable from main's history after the merge. A rebase that moves that parent
# must update this SHA. When it is not available (a shallow clone), the run is
# skipped, never guessed.
pre_fix_commit=f59824470b263aabb51abf4aef4bc9b19ca432d3
pre_fix_name="$(printf '%.12s' "$pre_fix_commit")"

printf '\n-- falsification: pre-fix %s must still exhibit the bugs\n' "$pre_fix_name"
old="$tmpd/old.sh"
if git -C "$here/.." show "$pre_fix_commit:scripts/$(basename "$installer")" > "$old" 2>/dev/null; then
    # The pin is only worth anything if the lifted code predates the guard: a
    # commit that already refuses symlinks would make the check below pass
    # while proving nothing, which is exactly how this block went vacuous.
    if grep -qF '[ -L "$shell_rc" ]' "$old"; then
        fail "pre-fix commit $pre_fix_name already carries the symlink guard; wrong reference"
    fi

    # Bug 0 (the one the file is named for): the service user plants a link at
    # the rc name, and root's append plus root's chown follow it into a file of
    # their choosing.
    box="$tmpd/old-symlink"; new_box "$box"
    printf 'SENTINEL: must not change\n' > "$box/secret"
    ln -s "$box/secret" "$box/.profile"
    if ! out="$(run_variant "$old" "$box" old 2>&1)"; then
        fail "pre-fix $pre_fix_name could not be lifted and run: $out"
    elif grep -qF "$box/install/bin" "$box/secret" 2>/dev/null; then
        pass "pre-fix $pre_fix_name appends through the planted symlink (bug reproduced)"
    else
        fail "pre-fix $pre_fix_name did NOT write through the symlink; this test proves nothing"
    fi
    if [ "$(id -u)" = 0 ]; then
        assert_eq "$svc" "$(owner_of "$box/secret")" \
            "pre-fix $pre_fix_name chowns the link target (bug reproduced)"
    else
        skip "chown-through-symlink needs root to observe"
    fi

    # Bug 1: the presence test is a plain grep, so a commented-out entry
    # suppresses the append permanently.
    box="$tmpd/old-commented"; new_box "$box"
    printf '# export PATH="%s/install/bin:$PATH"\nexport KEEP=1\n' "$box" > "$box/.profile"
    own_svc "$box/.profile"
    if ! out="$(run_variant "$old" "$box" old 2>&1)"; then
        # A lift that does not parse or no longer covers the pre-fix shape
        # would leave the rc untouched and make the check below pass for the
        # wrong reason, so say so instead of counting that as a reproduction.
        fail "pre-fix $pre_fix_name could not be lifted and run: $out"
    elif grep -qE '^[[:space:]]*export PATH' "$box/.profile"; then
        fail "pre-fix $pre_fix_name appended an active entry; this test proves nothing"
    else
        pass "pre-fix $pre_fix_name leaves the rc without an active PATH entry (bug reproduced)"
    fi

    # Bug 2: the entry is appended with no check for a trailing newline, so it
    # is glued onto the last line.
    box="$tmpd/old-nonewline"; new_box "$box"
    printf 'export KEEP=1' > "$box/.profile"
    own_svc "$box/.profile"
    if ! out="$(run_variant "$old" "$box" old 2>&1)"; then
        fail "pre-fix $pre_fix_name could not be lifted and run: $out"
    elif [ "$(head -1 "$box/.profile")" = 'export KEEP=1' ]; then
        fail "pre-fix $pre_fix_name did not glue the entry; this test proves nothing"
    else
        pass "pre-fix $pre_fix_name glues the PATH entry onto the last line (bug reproduced)"
    fi
else
    skip "pre-fix commit $pre_fix_name not available for the falsification run"
fi

printf '\n%s: %d failed, %d skipped\n' "$(basename "$0")" "$fails" "$skipped"
[ "$fails" -eq 0 ]
