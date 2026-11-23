# eventslab-relay

[events lab](https://events.kojira.io) で配信するための、手元の PC に入れる小さな中継アプリです。

ブラウザ（events lab の配信画面）から WebRTC（WHIP）で受け取った映像と音声を、RTMP で YouTube に送ります。

- **映像は作り直しません。** ブラウザがハードウェアエンコーダー（Mac は VideoToolbox、Windows は NVENC・QSV・AMF・Media Foundation など）で圧縮した H.264 を、そのまま YouTube に流します。
- **音声だけ変換します。** ブラウザの音声（Opus）を、YouTube が受け取る AAC に変えます。これには ffmpeg を使います。
- **PC の中だけで待ち受けます。** `127.0.0.1` だけで、外のネットワークには開きません。
- **配信キーはメモリにだけ置きます。** ファイルにもログにも残りません。

設計は events の [docs/browser-live-youtube.md](https://github.com/428lab/events/blob/docs/browser-live-youtube/docs/browser-live-youtube.md)（#11）にあります。

## ダウンロード

[Releases](https://github.com/428lab/eventslab-relay/releases) から、自分の PC に合うものを選んでください。

| PC | ファイル |
|---|---|
| Mac（Apple Silicon、M1 以降） | `eventslab-relay-darwin-arm64` |
| Mac（Intel） | `eventslab-relay-darwin-amd64` |
| Windows（64bit） | `eventslab-relay-windows-amd64.exe` |

`SHA256SUMS` で、ダウンロードしたファイルが壊れていないか確かめられます。

### ffmpeg が要ります（音声の変換）

今の版は ffmpeg を同梱していません（同梱は後の版で行います）。次のどちらかで用意してください。

- `PATH` の通った場所に ffmpeg を入れる（Mac なら `brew install ffmpeg`）。ふつうはこちら
- 置き場所を指定して起動する: `./eventslab-relay --ffmpeg /path/to/ffmpeg`

起動すると、使う ffmpeg の場所が画面に出ます。見つからないと注意が出て、配信を始められません。

探す順番は `--ffmpeg` → `PATH` → 中継アプリと同じフォルダ です。同じフォルダは最後にしてあります。
「ダウンロード」フォルダなどにたまたま置かれた別の `ffmpeg` を、勝手に使わないためです。

## 初めて起動するとき（署名していないアプリの警告）

今の版は、アプリの署名をしていません。そのため、初めて開くときに OS が警告を出します。

**Mac**

1. ダウンロードしたファイルに実行の権限を付けます：`chmod +x eventslab-relay-darwin-arm64`
2. Finder でファイルを右クリック（または control キーを押しながらクリック）→「開く」→ 確認の画面で「開く」。
   「開発元を確認できないため開けません」と出たときは、「システム設定」→「プライバシーとセキュリティ」の下の方にある「このまま開く」を押します。
3. ターミナルから起動しても構いません：`./eventslab-relay-darwin-arm64`

**Windows**

1. ダブルクリックで開きます。
2. 「Windows によって PC が保護されました」と出たら、「詳細情報」→「実行」を押します。
3. ファイアウォールの確認が出ることがあります。中継アプリは PC の中だけで通信するので、許可しなくても動きます。

## 使い方

1. 中継アプリを起動します。画面に次のように出ます。

   ```
   eventslab-relay v0.1.0
   待ち受け中 / Listening on http://127.0.0.1:47801
   ペアリング番号 / Pairing code: 123456
   ```

2. events lab の配信画面で「中継アプリとつなぐ」を押し、表示された 6 桁の番号を入れます（**ペアリング**）。
   - 1 回つないだら、次からは番号なしでつながります。
   - Chrome が「ローカルネットワークへのアクセスを許可しますか」と聞いてきたら、「許可」を押してください。
3. YouTube の配信キーを入れて、配信を始めます。
4. 配信を止めるときは、events lab の画面で止めます。中継アプリは開いたままで構いません。

中継アプリのウィンドウ（ターミナル）を閉じると、配信は止まります。

### 起動のオプション

| オプション | 意味 |
|---|---|
| `--port 47805` | このポートだけで待ち受ける（既定は 47801、使われていたら 47802〜47809 を順に試す） |
| `--ffmpeg /path/to/ffmpeg` | 使う ffmpeg を指定する |
| `--dev` | 開発用。`http://localhost` と `http://127.0.0.1` のページからも使えるようにする |
| `--rtmp-test rtmp://…/app/key` | ブラウザ無しで、試験用の映像と音を 10 秒送る（つながるかの確認用。ハードウェアエンコーダーで作る） |
| `--version` | 版を出す |

## うまくいかないとき

| こうなる | 確かめること |
|---|---|
| events lab が「中継アプリが動いていません」と言う | 中継アプリが起動しているか。ほかのアプリが 47801〜47809 を全部使っていないか |
| 「ローカルネットワークへのアクセス」を拒否してしまった | Chrome のアドレスバーの左のアイコン →「サイトの設定」→「ローカル ネットワーク アクセス」を「許可」に |
| ペアリングの番号が合わない | 5 回間違えると番号が変わります。中継アプリの画面の新しい番号を入れてください。3 回続けて間違えると、少しのあいだ（1〜30 秒）番号を受け付けません。少し待ってから入れ直してください |
| 「ffmpeg が見つかりません」 | 上の「ffmpeg が要ります」を見てください |
| YouTube に映らない | 配信キーが合っているか。中継アプリの画面に `rtmp:` から始まる失敗の行が出ていないか（自動でつなぎ直します） |
| 映像がカクカクする・PC が重い | events lab の配信画面に「ハードウェアエンコーダーが使われていません」と出ていないか。Chrome の `chrome://gpu` で「Video Encode: Hardware accelerated」になっているか |

## 中継アプリの API（events lab 向け）

`http://127.0.0.1:<port>` で待ち受けます。`/v1/hello` と `/v1/pair` 以外は、ペアリングでもらったトークンを `Authorization: Bearer <token>` で付けます。
宛先（`Host`）が `127.0.0.1:<port>` か `localhost:<port>` でない要求は、421 で断ります（DNS リバインディング対策）。
応答を読めるのは、許可した origin（`https://events.kojira.io` と `https://eventer-staging.kojiran.workers.dev`）のページだけです。

| メソッドとパス | 中身 |
|---|---|
| `GET /v1/hello` | `{ app, version, paired }` |
| `POST /v1/pair` | `{ origin, code }` → `{ token }`（403 は番号違いか、許可していない origin。429 は間違いが続いたので少し待つ。`Retry-After` に秒数） |
| `POST /v1/start` | `{ rtmpUrl, streamKey }` → `{ whipUrl }`。鍵はメモリにだけ置く |
| `POST /v1/whip` | WHIP（`Content-Type: application/sdp`）。201 と `Location: /v1/whip/<id>` |
| `DELETE /v1/whip/<id>` | 配信を止める |
| `GET /v1/status` | `{ state, rtmp, error?, ffmpeg, videoKbps, audioKbps, fps, droppedFrames, rtmpReconnects, h264Profile, … }` |

ペアリングのトークンは、ハッシュにして次の場所に保存します。

- Mac: `~/Library/Application Support/eventslab-relay/tokens.json`
- Windows: `%AppData%\eventslab-relay\tokens.json`

このファイルを消すと、ペアリングをやり直せます。

## 作る・試す

```sh
go test ./...        # ffmpeg があれば、WebRTC → RTMP の通しのテストも走る
go build -o eventslab-relay .
```

タグ `v*` を push すると、GitHub Actions が Mac（arm64・amd64）と Windows（amd64）向けに作り、Releases に置きます。

## ライセンス

MIT（[LICENSE](LICENSE)）。

使っている部品：

| 部品 | ライセンス |
|---|---|
| [pion/webrtc](https://github.com/pion/webrtc) ほか pion の部品 | MIT |
| [yutopp/go-rtmp](https://github.com/yutopp/go-rtmp)・go-amf0 | BSL-1.0 |
| [sirupsen/logrus](https://github.com/sirupsen/logrus) | MIT |
| google/uuid, pkg/errors, golang.org/x/*, wlynxg/anet | BSD-3-Clause（pkg/errors は BSD-2-Clause） |
| hashicorp/errwrap, go-multierror | MPL-2.0 |
| mitchellh/mapstructure | MIT |

ffmpeg は同梱していません（別に入れてもらう外部のプログラムです）。将来同梱するときは、LGPL のビルド（GPL の部品と fdk-aac を含まないもの）にして、ライセンスの表示を付けます。
