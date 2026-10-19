# Установка 3x-ui с ядерным датапасом AmneziaWG

Целевая ОС: **Debian 13 (trixie)**, x86_64, root, обычный VPS или железо —
**не Docker** (см. §2). Ветка: `feat/awg-kernel-backend-v2`.

Мануал делится на две независимые половины: обычная панель (§3–§4) и перевод
одного инбаунда на модуль ядра (§5–§8). Режим ядра включается флажком на
инбаунде и в любой момент выключается обратно (§11).

---

## 1. Сначала прочитайте это — возможно, ядро вам не нужно

Замеры, ради которых всё затевалось (124 → 190 Мбит/с на 1 vCPU), сняты
**в августе**. После них в апстрим влились два изменения, бьющие ровно в то же
место:

- `tunQueueDepth` в gVisor-стеке поднят с 1024 до 8192 (×8);
- H1–H4 генерируются одиночными значениями вместо диапазонов: с включённым
  `RandomTrailers` широкий диапазон заставлял приёмник принимать транспортные
  пакеты за хендшейки и терять их.

Честная формулировка: **механизм потолка встроенного датапаса так и не
установлен**. Три гипотезы (CPU, глубина очереди, TCP-буферы) проверены и
отвергнуты. Поэтому порядок такой:

1. поставьте обычную панель (§3) и снимите скорость на большом файле;
2. если хватает — остановитесь: сэкономите себе DKMS, Secure Boot и пересборку
   модуля при каждом обновлении ядра;
3. если нет — продолжайте с §4.

Что теряется при уходе в ядро — в §9. Это не мелочи.

---

## 2. Требования к хосту

| Требование | Проверка | Если не выполнено |
|---|---|---|
| root | `id -u` → `0` | панель обязана работать от root: она создаёт сетевой интерфейс |
| не Docker | — | образ панели на Alpine: нет ни `awg`, ни `nft`, ни `dkms`, а установщик модуля написан под `apt-get`. Модуль вдобавок нужен на **хосте**, контейнеру понадобились бы `--privileged` и host netns. Не поддерживается |
| заголовки ядра | `ls -d /lib/modules/$(uname -r)/build` | `apt-get install linux-headers-$(uname -r)` |
| свободный UDP-порт | `ss -ulnp \| grep :<порт>` | выберите другой |
| Secure Boot выключен **или** есть ключ подписи | `mokutil --sb-state` | неподписанный DKMS-модуль не загрузится |

Одной командой:

```bash
id -u; uname -r
ls -d /lib/modules/$(uname -r)/build 2>&1
mokutil --sb-state 2>/dev/null || echo "mokutil нет — Secure Boot скорее всего выключен"
```

---

## 3. Обычная установка панели

Ставим штатным инсталлятором, даже если потом заменим бинарь: он приносит то,
чего в самосборном бинаре нет — каталог `/usr/local/x-ui/bin/` с xray-core,
шестью geo-файлами `.dat` и `mtg-linux-*`, systemd-юнит, `/etc/x-ui/` под базу и
`/usr/bin/x-ui` (управляющий скрипт).

```bash
bash <(curl -Ls https://raw.githubusercontent.com/MHSanaei/3x-ui/main/install.sh)
```

Логин, пароль, порт и base path он сгенерирует сам и продублирует в
`/etc/x-ui/install-result.env` (режим 600):

```bash
cat /etc/x-ui/install-result.env
```

Проверка:

```bash
systemctl is-active x-ui                    # active
/usr/local/x-ui/x-ui -v                     # версия
/usr/local/x-ui/x-ui setting -show          # hasDefaultCredential: false

. /etc/x-ui/install-result.env
curl -o /dev/null -w '%{http_code}\n' \
  "http://127.0.0.1:${XUI_PANEL_PORT}/${XUI_WEB_BASE_PATH}/"   # 200 или 30x
```

Зайдите в панель, заведите AmneziaWG-инбаунд и клиента, подключитесь и **снимите
скорость**. Это база сравнения из §1.

---

## 4. Сборка форк-бинаря

Кросс-сборка с Windows не поддерживается: драйвер SQLite (`mattn/go-sqlite3`)
требует cgo и компилятора под целевую платформу. Собирайте на Linux — проще
всего прямо на целевом сервере.

> **Откуда брать код.** Ветка `feat/awg-kernel-backend-v2` опубликована в форке
> YoungReckless4/3x-ui и содержит ядровой режим вместе с актуальными изменениями
> upstream. Её можно сразу клонировать и собрать по §4.1–4.2.
>
> Если нужно использовать готовый бинарь (`scp`), пропустите §4.1–4.2 и
> переходите к §4.3. Это исторический бинарь из коммита
> `4243b63`, собранный до обновления ветки до актуального `main`; он не содержит
> последующие изменения upstream. Хэш подтверждает только этот старый бинарь,
> а не текущий исходный код:
>
> ```
> размер   72 270 160 байт
> sha256   bb62c592133acfb63809dde3267aaf1bba0dbf67152fe55cb61521e833aaa77c
> ```
>
> Сверьте на сервере: `sha256sum x-ui`. Бинарь динамически слинкован с glibc
> Debian 12 — на Debian 13 запускается (glibc совместим вперёд), на Alpine или
> другой musl-системе работать не будет.

### 4.1 Инструменты

```bash
apt-get update
apt-get install -y build-essential git curl ca-certificates

# Node 26 — нужен для сборки фронтенда
curl -fsSL https://deb.nodesource.com/setup_26.x | bash -
apt-get install -y nodejs

# Go 1.27.2+ — в репозиториях Debian 13 версия старее, берём с go.dev
GO_TARBALL=go1.27.2.linux-amd64.tar.gz      # сверьтесь с https://go.dev/dl/
curl -fLO "https://go.dev/dl/${GO_TARBALL}"
rm -rf /usr/local/go && tar -C /usr/local -xzf "${GO_TARBALL}"
export PATH=/usr/local/go/bin:$PATH

go version && node --version                # go1.27.2+, v26.x
```

`go.mod` требует ровно `go >= 1.27.2`; более старый Go остановится на этой
строке ещё до компиляции.

### 4.2 Сборка

Порядок обязателен: Go встраивает каталог `internal/web/dist/` директивой
`//go:embed`, поэтому фронтенд собирается **первым** — иначе сборка падает с
`pattern all:dist: no matching files found`.

```bash
git clone https://github.com/YoungReckless4/3x-ui.git /usr/local/src/3x-ui-fork
cd /usr/local/src/3x-ui-fork
git checkout feat/awg-kernel-backend-v2

cd frontend && npm ci && npm run build       # → ../internal/web/dist/
cd ..
CGO_ENABLED=1 go build -ldflags "-w -s" -o x-ui .

./x-ui -v
```

`make build` для этого не годится: он вызывает `go build ./...`, который
компилирует пакеты, но исполняемый файл не пишет.

### 4.3 Подмена бинаря

```bash
systemctl stop x-ui
cp /usr/local/x-ui/x-ui /root/x-ui.orig.bak             # для отката
install -m 755 -o root -g root x-ui /usr/local/x-ui/x-ui
systemctl start x-ui

systemctl is-active x-ui && /usr/local/x-ui/x-ui -v
```

Не трогайте `/usr/local/x-ui/bin/`, `/etc/x-ui/x-ui.db` и `/root/cert/` — там
xray-core с geo-файлами, база и сертификаты.

> **Предупреждение.** Пункты меню `x-ui` → `update` и `update-dev` (равно как
> `x-ui update` из командной строки) скачивают официальный релиз и **молча
> затирают** ваш бинарь вместе с ядерным режимом. После такого обновления
> повторите §4.2–4.3. Обычный `apt upgrade` бинарь не трогает.

---

## 5. Установка модуля ядра

```bash
sudo /usr/local/x-ui/x-ui install-awg-kernel
```

Именно полным путём: `/usr/bin/x-ui` — это управляющий **shell-скрипт**, такой
подкоманды у него нет, он просто откроет меню.

Скрипт зашит в бинарь и делает следующее:

1. проверяет root и наличие `/lib/modules/$(uname -r)/build`;
2. `apt-get install -y dkms build-essential git libmnl-dev pkg-config nftables`;
   если ядро собрано clang'ом (`grep -i clang /proc/version`) — ещё
   `clang lld llvm`, иначе DKMS запустит сборку с `LLVM=1` и упадёт;
3. клонирует модуль по последнему тегу `^v3.1` и `amneziawg-tools` по `^v3.`.
   **Обе части обязаны быть из одной серии 3.x**: инструменты 1.0 шлют H1–H4
   как u32, и `awg setconf` падает с `Unable to modify interface: Invalid
   argument`, оставляя интерфейс поднятым и неработающим без внятной причины;
4. на ядрах ≥ 7.1.5 патчит `src/socket.c` под новую сигнатуру
   `setup_udp_tunnel_sock` / `udp_tunnel_sock_release`;
5. `dkms add/build/install -m amneziawg -v 1.0.0`, затем `modprobe amneziawg`;
6. собирает и ставит `awg` и `awg-quick`.

Проверка:

```bash
dkms status -m amneziawg          # amneziawg/1.0.0, <ядро>, x86_64: installed
lsmod | grep '^amneziawg'
awg --version
command -v awg-quick nft
```

---

## 6. Включение режима ядра на инбаунде

1. Панель → нужный AmneziaWG-инбаунд → **Режим ядра**.
2. Если переключатель заблокирован, под ним список того, чего не хватает, и
   готовая к копированию команда. Проверка готовности хоста возвращает коды
   `root`, `tools`, `module`, `nft`.
3. Сохраните. Панель проверит хост ещё раз на сохранении — форма может показывать
   устаревшее состояние, а API дёргают и напрямую.
4. Реконсайл идёт раз в 10 секунд; интерфейс поднимется в пределах этого окна.

На сохранении kernel mode дополнительно блокируется, если включён IPv6, настроен
проброс порта хотя бы у одного клиента или включён `routeThroughXray`. Это
проверки инбаунда, а не коды готовности хоста. Конкретный адрес `Listen` также
нельзя использовать в режиме ядра: `awg-quick` не поддерживает привязку к
заданному адресу и иначе поднял бы сокет на всех интерфейсах. Во встроенном
режиме такой адрес поддерживается.

**Конфиги клиентов менять не нужно.** Все три генератора (подписка на Go,
скачивание из панели, share-ссылка) читают только настройки инбаунда и клиента и
о датапасе ничего не знают — файл получается тот же байт в байт.

---

## 7. Файрвол — панель этого не делает

Генерируемый `PostUp` создаёт только NAT:

```
sysctl -q -w net.ipv4.ip_forward=1
nft add table ip awgnat<N>
nft add chain ip awgnat<N> post '{ type nat hook postrouting priority 100 ; }'
nft add rule  ip awgnat<N> post ip saddr <подсеть> oifname "<внешний NIC>" masquerade
```

Цепочку `filter forward` он не трогает и UDP-порт инбаунда не открывает. На
чистом Debian без файрвола это неважно: таблицы `inet filter` попросту нет, а
политика форвардинга ядра по умолчанию `ACCEPT`. Но если у вас ufw, firewalld
или наследие Docker с policy `drop`, туннель поднимется, хендшейк пройдёт — и
**не пропустит ни байта**. Это самый вероятный симптом «всё зелёное, но
интернета нет».

```bash
nft list chain inet filter forward 2>/dev/null | head   # если пусто — таблицы нет, всё в порядке
iptables -S FORWARD 2>/dev/null | head                  # policy DROP — надо открывать
```

Если политика запрещающая:

```bash
N=<id инбаунда>
nft add rule inet filter forward iifname "awg$N" accept
nft add rule inet filter forward oifname "awg$N" ct state established,related accept
nft add rule inet filter input udp dport <порт инбаунда> accept
```

Правила `nft` не переживают перезагрузку — закрепите их в `/etc/nftables.conf`
(`systemctl enable nftables`) или в правилах вашего файрвола. То же с
`net.ipv4.ip_forward`: `PostUp` выставляет его при каждом подъёме интерфейса, но
в `/etc/sysctl.d/` ничего не пишет.

---

## 8. Проверка

```bash
N=<id инбаунда>                          # он же номер в имени интерфейса

dkms status -m amneziawg
lsmod | grep '^amneziawg'
ip -brief addr show awg$N                # UP, адрес из подсети туннеля
cat /sys/class/net/awg$N/mtu             # ваш MTU, либо 1420-S4, либо 1420
awg show awg$N                           # listening port, пиры, latest handshake
nft list table ip awgnat$N               # правило masquerade
ls -l /etc/amnezia/amneziawg/awg$N.conf  # -rw------- (внутри приватный ключ)
journalctl -u x-ui -n 50 | grep -i amneziawgkern
```

Признак живого туннеля — растущие счётчики у пира в `awg show awg$N` и
`latest handshake` не старше пары минут. После этого снимите скорость тем же
способом, что и в §3, и сравните.

---

## 9. Что перестаёт работать в режиме ядра

Пиры такого инбаунда **не проходят через Xray вообще**: трафик форвардит ядро и
выпускает через NAT. Отсюда:

| Возможность | В режиме ядра |
|---|---|
| Правила маршрутизации Xray, sniffing, per-client routing | **не применяются** |
| Учёт трафика | работает: снимается из `awg show dump` дельтами раз в 10 с |
| Онлайн-статус | по хендшейку свежее 3 минут, а не по сессиям Xray |
| Лимит IP | адреса подключений видны (окно 30 минут), но **отключить нарушителя нельзя** — для amneziawg это не поддержано и во встроенном датапасе |
| IPv6 на пира | **запрещено** на сохранении |
| Проброс портов клиенту | **запрещено** на сохранении |
| `routeThroughXray` | **запрещено** на сохранении: мост TPROXY не реализован, а NAT при нём не создаётся |
| Лимит трафика и срок действия | работают: превысивший клиент отключается в БД, следующий тик убирает его пир из интерфейса |

---

## 10. Эксплуатация

- **Обновление ядра.** DKMS пересобирает модуль автоматически, но проверяйте
  после `apt upgrade` и перезагрузки: `dkms status -m amneziawg`,
  `lsmod | grep amneziawg`. При Secure Boot модуль надо подписать заново.
- **Перезапуск панели туннели не роняет.** Это сделано намеренно: простаивающий
  пир без `PersistentKeepalive` не имеет чем инициировать переподключение и
  после обрыва остался бы отключённым. При старте панель усыновляет уже
  поднятый интерфейс, если адрес и MTU совпадают с ожидаемыми.
- **Обратная сторона того же.** После удаления или остановки панели остаются
  живые интерфейсы `awgN`, файлы `/etc/amnezia/amneziawg/*.conf` с приватными
  ключами и таблицы `awgnat<N>`. Убирать вручную — см. §11.
- **`PersistentKeepalive`** у клиентов имеет смысл выставить в 25: он держит
  NAT-маппинг открытым и возвращает пир сам после любого разрыва.

---

## 11. Откат

**На встроенный датапас** — обратимо, клиентов не трогает:

```bash
# 1. Снять «Режим ядра» на инбаунде в панели и сохранить.
# 2. Дождаться реконсайла (≤10 с), затем убрать остатки:
N=<id инбаунда>
awg-quick down awg$N 2>/dev/null || ip link del awg$N
nft delete table ip awgnat$N 2>/dev/null
rm -f /etc/amnezia/amneziawg/awg$N.conf
```

**На официальный бинарь.** Снимите флажок ДО отката: иначе в настройках
останется `kernelMode`, которого официальный бинарь не знает, и инбаунд молча
уедет на встроенный датапас, пока живой `awgN` держит тот же UDP-порт.

```bash
systemctl stop x-ui
install -m 755 -o root -g root /root/x-ui.orig.bak /usr/local/x-ui/x-ui
systemctl start x-ui
```

**Модуль целиком:**

```bash
modprobe -r amneziawg
dkms remove -m amneziawg -v 1.0.0 --all
```

---

## 12. Приложение: ядерный туннель без форк-бинаря

Если собирать бинарь не хочется, тот же датапас поднимается руками: панель
остаётся хранилищем ключей и генератором клиентских конфигов, а интерфейс
поднимает `awg-quick`.

**Инбаунд в панели при этом должен быть выключен** — иначе встроенный датапас
займёт тот же UDP-порт.

```bash
NIC=$(ip -o -4 route show default | awk '{print $5; exit}')
export NIC
python3 - <<'PY'
import sqlite3, json, os
DB = '/etc/x-ui/x-ui.db'
row = sqlite3.connect(DB).execute(
    "select port, settings from inbounds where protocol='amneziawg'").fetchone()
port, s = row[0], json.loads(row[1])
srv, clients = s['server'], s['clients']
gw = srv['subnetIp'].rsplit('.', 1)[0] + '.1'
subnet = "%s/%d" % (srv['subnetIp'], srv['subnetCidr'])
mtu = srv.get('mtu') or max(1420 - srv.get('s4', 0), 1280)
L = ["[Interface]",
     "PrivateKey = %s" % srv['privateKey'],
     "Address = %s/%d" % (gw, srv['subnetCidr']),
     "ListenPort = %d" % port,
     "MTU = %d" % mtu]
for cfg, key in (("Jc","jc"),("Jmin","jmin"),("Jmax","jmax"),
                 ("S1","s1"),("S2","s2"),("S3","s3"),("S4","s4")):
    L.append("%s = %s" % (cfg, srv[key]))
for cfg, key in (("H1","h1"),("H2","h2"),("H3","h3"),("H4","h4"),
                 ("I1","i1"),("I2","i2"),("I3","i3"),("I4","i4"),("I5","i5"),
                 ("HeaderProtectionKey","headerProtectionKey"),
                 ("ContentPaddingAddition","contentPaddingAddition"),
                 ("RekeyAfterTime","rekeyAfterTime"),("RekeyTimeout","rekeyTimeout"),
                 ("RejectAfterTime","rejectAfterTime"),("KeepaliveTimeout","keepaliveTimeout"),
                 ("MaxHandshakeAttempts","maxHandshakeAttempts")):
    v = srv.get(key, "")
    if v not in ("", None):
        L.append("%s = %s" % (cfg, v))
for cfg, key in (("RandomTrailers","randomTrailers"), ("DisableCookies","disableCookies")):
    if srv.get(key):
        L.append("%s = on" % cfg)
nic = os.environ.get('NIC') or 'eth0'
L += ["PostUp = sysctl -q -w net.ipv4.ip_forward=1",
      "PostUp = nft add table ip awgnat",
      "PostUp = nft add chain ip awgnat post '{ type nat hook postrouting priority 100 ; }'",
      'PostUp = nft add rule ip awgnat post ip saddr %s oifname "%s" masquerade' % (subnet, nic),
      "PostDown = nft delete table ip awgnat"]
for c in clients:
    if not c.get('enable', True):
        continue
    L += ["", "[Peer]",
          "# %s %s" % (c.get('email', ''), c.get('comment', '')),
          "PublicKey = %s" % c['publicKey'],
          "AllowedIPs = %s" % ", ".join(c['allowedIPs'])]
os.makedirs('/etc/amnezia/amneziawg', exist_ok=True)
p = '/etc/amnezia/amneziawg/awg0.conf'
open(p, 'w').write("\n".join(L) + "\n")
os.chmod(p, 0o600)
print("written %s: %d peers, port %d, mtu %d" %
      (p, len([c for c in clients if c.get('enable', True)]), port, mtu))
PY

awg-quick up awg0

# Автозапуск, если amneziawg-tools поставили systemd-юнит:
systemctl list-unit-files 'awg-quick@*' && systemctl enable awg-quick@awg0
```

Если юнита `awg-quick@.service` нет (его установка зависит от того, как собраны
tools), заведите свой или поднимайте интерфейс из `/etc/rc.local`.

Ограничения тут шире, чем в §6: панель не увидит ни трафик, ни онлайн-статус
таких пиров и **не будет пересобирать конфиг при правке клиентов**. После
каждого изменения запускайте скрипт заново и применяйте:

```bash
awg syncconf awg0 <(awg-quick strip awg0)
```
