#!/bin/sh
# Local JWT payload validation helper for UrNetwork container scripts.
# Validates exp and nbf claims with a 5-minute (300s) clock skew.
# Does not log or expose the token.

validate_jwt_file() {
  jwt_path="$1"
  [ -f "$jwt_path" ] && [ -s "$jwt_path" ] || return 1

  # Read the token (trim whitespace and newlines)
  token_str="$(head -n 1 "$jwt_path" 2>/dev/null | tr -d '\r\n[:space:]')"
  [ -n "$token_str" ] || return 1

  # A valid JWT must consist of exactly 3 dot-separated segments
  num_parts="$(printf '%s' "$token_str" | awk -F. '{print NF}')"
  [ "$num_parts" -eq 3 ] || return 1

  # Header and payload must be non-empty
  header_seg="$(printf '%s' "$token_str" | cut -d. -f1)"
  payload_seg="$(printf '%s' "$token_str" | cut -d. -f2)"
  [ -n "$header_seg" ] && [ -n "$payload_seg" ] || return 1

  # Convert base64url to standard base64 and pad with '='
  b64_payload="$(printf '%s' "$payload_seg" | tr -- '-_' '+/')"
  pad_rem=$((${#b64_payload} % 4))
  case "$pad_rem" in
    2) b64_payload="${b64_payload}==" ;;
    3) b64_payload="${b64_payload}=" ;;
    1) return 1 ;; # Invalid base64 length
    0) ;;
  esac

  # Decode payload
  decoded_json="$(printf '%s' "$b64_payload" | base64 -d 2>/dev/null)" || return 1
  [ -n "$decoded_json" ] || return 1

  # Validate JSON structure with jq
  if ! printf '%s' "$decoded_json" | jq empty 2>/dev/null; then
    return 1
  fi

  # Extract exp and nbf claims
  exp_claim="$(printf '%s' "$decoded_json" | jq -r '.exp // empty' 2>/dev/null)"
  nbf_claim="$(printf '%s' "$decoded_json" | jq -r '.nbf // empty' 2>/dev/null)"

  curr_time="$(date +%s 2>/dev/null)" || return 1
  skew_sec=300 # 5 minutes clock skew

  # Validate exp (expiration time) if present
  if [ -n "$exp_claim" ]; then
    case "$exp_claim" in
      ''|*[!0-9]*) return 1 ;;
    esac
    if [ "$((curr_time - skew_sec))" -gt "$exp_claim" ]; then
      return 1
    fi
  fi

  # Validate nbf (not before) if present
  if [ -n "$nbf_claim" ]; then
    case "$nbf_claim" in
      ''|*[!0-9]*) return 1 ;;
    esac
    if [ "$((curr_time + skew_sec))" -lt "$nbf_claim" ]; then
      return 1
    fi
  fi

  return 0
}
