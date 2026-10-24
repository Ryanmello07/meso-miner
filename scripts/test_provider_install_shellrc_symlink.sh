#!/bin/bash
# shellcheck disable=SC1090,SC2016,SC2034,SC2329
# The block under test is lifted out of the installer and eval'd in this scope,
# so it sources $lib, reads every variable assigned below and calls user_shell();
# ShellCheck cannot follow any of that. SC2016 fires on grep patterns that are
# meant to match literal text in the lifted block.
#
# Prove the FreeBSD installer's PATH hint never writes through a symlinked
# shell rc. The service user owns the directory the rc file lives in, so a
# link planted there used to redirect root's append *and* root's chown into a
# file of their choosing. A symlinked rc must now be refused untouched, and a
# normal one must gain the PATH line exactly once via rename, not append.
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

# chown can only really change an owner as root, and root is the account whose
# write the symlink hijacks, so re-run the suite under sudo when we can.
if [ "$(id -u)" != 0 ] && [ "${SHELLRC_TEST_NO_SUDO:-0}" != 1 ] && sudo -n true 2>/dev/null; then
    exec sudo -n env SHELLRC_TEST_NO_SUDO=1 TMPDIR="${TMPDIR:-/tmp}" \
        "$here/$(basename "$0")" "$@"
fi

tmpd="$(mktemp -d "${TMPDIR:-/tmp}/shellrc-test.XXXXXX")" || exit 1
trap 'rm -rf "$tmpd"' EXIT

svc="$(id -un)"
if [ "$(id -u)" = 0 ]; then
    # A real unprivileged account, so the chown is a genuine ownership change.
    for candidate in nobody nfsnobody daemon; do
        if id "$candidate" > /dev/null 2>&1; then svc="$candidate"; break; fi
    done
fi
printf '== %s (uid %s, service user %s)\n' "$(basename "$here")" "$(id -u)" "$svc"

owner_of() { stat -c '%U' "$1" 2>/dev/null || stat -f '%Su' "$1" 2>/dev/null; }
mode_of()  { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null; }

# run_variant <installer> <sandbox> <expect> — expect is "guard" for the fixed
# file (must refuse symlinks) or "append" for the old one (must not).
run_variant() {
    local inst="$1" box="$2" expect="$3" lib="$2/.lib.sh" blk="$2/.block.sh"
    sed '/^case "$operation" in/,$d' "$inst" > "$lib"
    # From the shell_rc assignment to just before the 4-space `fi` that closes
    # the UPDATING block. That closer's opener is outside the lift, so printing
    # it would leave the fragment unbalanced. Structure is identical in the old
    # and new versions.
    awk '
        index($0, "_shell=\"$(user_shell") > 0 { on = 1 }
        on && $0 == "    fi" { exit }
        on { print }
    ' "$inst" > "$blk"

    # A truncated or unbalanced lift must fail here, not eval into a syntax
    # error after doing half the work.
    if ! sh -n "$blk" 2>/dev/null; then
        echo "EXTRACT-BROKEN: lifted block does not parse" >&2
        return 1
    fi

    # Fail loudly if the extraction ever stops covering the code under test,
    # instead of silently passing on an empty block.
    if [ "$expect" = guard ] && ! grep -qF '[ -L "$shell_rc" ]' "$blk"; then
        echo "EXTRACT-MISSING: symlink guard not in lifted block" >&2
        return 1
    fi
    if [ "$expect" = append ] && ! grep -qF '>> "$shell_rc"' "$blk"; then
        echo "EXTRACT-MISSING: direct append not in lifted block" >&2
        return 1
    fi

    (
        . "$lib"
        # The stripped tail ends with `[ -n "$operation" ] && shift`, which
        # returns 1 when sourced with no args; `:` keeps that from looking
        # like a failed source.
        :
        user_shell() { echo /bin/sh; }
        install_path="$box/install"
        service_user="$svc"
        service_home="$box"
        UPDATING=0
        eval "$(cat "$blk")"
    ) 2>&1
}

printf '\n-- a planted symlink is refused\n'
box="$tmpd/symlink"; mkdir -p "$box"
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
box="$tmpd/normal"; mkdir -p "$box"
printf 'export KEEP=1\n' > "$box/.profile"
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
box="$tmpd/missing"; mkdir -p "$box"
run_variant "$installer" "$box" guard > /dev/null
if [ -f "$box/.profile" ] && [ ! -L "$box/.profile" ]; then
    pass "the rc file was created as a regular file"
else
    fail "the rc file was not created"
fi
assert_eq 1 "$(grep -cF "$box/install/bin" "$box/.profile")" "the created file has one PATH line"

printf '\n-- falsification: the pre-fix version must still write through\n'
old="$tmpd/old.sh"
if git -C "$here/.." show "origin/main:scripts/$(basename "$installer")" > "$old" 2>/dev/null; then
    box="$tmpd/old"; mkdir -p "$box"
    printf 'SENTINEL: must not change\n' > "$box/secret"
    ln -s "$box/secret" "$box/.profile"
    run_variant "$old" "$box" append > /dev/null

    if grep -qF "$box/install/bin" "$box/secret" 2>/dev/null; then
        pass "origin/main appends through the planted symlink (bug reproduced)"
    else
        fail "origin/main did NOT write through the symlink; this test proves nothing"
    fi
    if [ "$(id -u)" = 0 ]; then
        assert_eq "$svc" "$(owner_of "$box/secret")" \
            "origin/main chowns the link target (bug reproduced)"
    else
        skip "chown-through-symlink needs root to observe"
    fi
else
    skip "origin/main not available for the falsification run"
fi

printf '\n%s: %d failed, %d skipped\n' "$(basename "$0")" "$fails" "$skipped"
[ "$fails" -eq 0 ]
