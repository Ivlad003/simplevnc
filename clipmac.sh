#!/bin/bash
# SimpleVNC: автоматичний спільний буфер обміну (текст) між цим Mac і ПК.
# Запуск:  curl -s __BASE__/clip/mac.sh | bash
# Скопіювали текст на Mac — він у буфері ПК, і навпаки. Зупинити: Ctrl+C.
# Потрібні лише штатні програми macOS: curl, pbcopy, pbpaste, shasum.

# Усе загорнуто у функцію, яка викликається останнім рядком: так сценарій
# виконується лише після того, як завантажився повністю.
main() {
BASE="__BASE__"
export LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
PBPASTE=${SIMPLEVNC_PBPASTE:-pbpaste}
PBCOPY=${SIMPLEVNC_PBCOPY:-pbcopy}
JAR=$(mktemp "${TMPDIR:-/tmp}/simplevnc-clip.XXXXXX")
NL=$'\n'

stop() { rm -f "$JAR"; echo; echo "Синхронізацію зупинено."; exit 0; }
trap stop INT TERM

say() { echo "$(date +%H:%M:%S)  $*"; }

sha() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 | cut -d' ' -f1; else sha256sum | cut -d' ' -f1; fi
}

# Вхід: пароль мережею не передається — лише його хеш з одноразовим числом від ПК.
login() {
  local info nonce proof code
  info=$(curl -s -m 10 -c "$JAR" -b "$JAR" "$BASE/clip/api/nonce") || return 1
  case "$info" in
    *'"password":false'*) return 0 ;;
    *'"nonce"'*) ;;
    *) return 1 ;;
  esac
  nonce=$(printf '%s' "$info" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  proof=$(printf '%s' "$nonce:$PASSWORD" | sha)
  code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -c "$JAR" -b "$JAR" \
    -H 'Content-Type: application/json' -d "{\"nonce\":\"$nonce\",\"proof\":\"$proof\"}" \
    "$BASE/clip/api/login")
  if [ "$code" = 429 ]; then say "Забагато невдалих спроб. Зачекайте хвилину."; fi
  [ "$code" = 204 ]
}

PASSWORD=${SIMPLEVNC_PASSWORD:-}
if [ -z "$PASSWORD" ]; then
  printf 'Пароль SimpleVNC: ' > /dev/tty
  read -r -s PASSWORD < /dev/tty
  echo > /dev/tty
fi
if ! login; then
  echo "Не вдалося увійти: невірний пароль або ПК $BASE недосяжний."
  rm -f "$JAR"
  exit 1
fi
say "Підключено до $BASE. Текст, скопійований від цієї миті, синхронізується в обидва боки."
say "Файли передаються через сторінку $BASE/clip/ у браузері. Зупинити: Ctrl+C."

last_local=$($PBPASTE 2>/dev/null)
last_seq=""
offline=0
while true; do
  cur=$($PBPASTE 2>/dev/null)
  resp=$(curl -s -m 10 -b "$JAR" -w "${NL}%{http_code}" "$BASE/clip/api/plain")
  code=${resp##*$NL}
  body=${resp%$NL*}
  if [ "$code" = 401 ]; then # сеанс закінчився або ПК перезапустився
    login || sleep 3
    continue
  fi
  if [ "$code" != 200 ]; then
    if [ "$offline" = 0 ]; then say "Немає зв'язку з ПК. Пробую далі."; offline=1; fi
    sleep 3
    continue
  fi
  if [ "$offline" = 1 ]; then say "Зв'язок із ПК відновлено."; offline=0; fi
  seq=${body%%$NL*}
  text=${body#*$NL}
  if [ -z "$last_seq" ]; then # перший прохід: лише запам'ятовуємо, що де лежить
    last_seq=$seq
    last_local=$cur
  elif [ "$cur" != "$last_local" ]; then
    last_local=$cur
    if [ -n "$cur" ] && [ "$cur" != "$text" ]; then
      printf '%s' "$cur" | curl -s -m 30 -b "$JAR" -o /dev/null \
        -H 'Content-Type: text/plain; charset=utf-8' --data-binary @- "$BASE/clip/api/text"
      # наше надсилання змінило буфер ПК — запам'ятовуємо його номер, щоб не тягнути текст назад
      last_seq=$(curl -s -m 10 -b "$JAR" "$BASE/clip/api/plain" | head -n 1)
      say "Mac → ПК: ${#cur} символів"
    fi
  elif [ "$seq" != "$last_seq" ]; then
    last_seq=$seq
    if [ -n "$text" ] && [ "$text" != "$cur" ]; then
      printf '%s' "$text" | $PBCOPY
      last_local=$($PBPASTE 2>/dev/null)
      say "ПК → Mac: ${#text} символів"
    fi
  fi
  sleep 1
done
}

main
