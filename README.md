# switchbot-panel

LAN 内の PC ブラウザから SwitchBot デバイス一覧と基本操作を行うための小さい Web アプリです。

機器をカードで並べ、画面幅に合わせて列数を変えます。温湿度計は一覧で温度・湿度を表示します。操作は機器別のアイコン付きボタン、明るさ・色温度・位置のスライダー、カラーピッカーで行います。照明の明るさ・色温度・色、カーテンの位置、ブラインドの角度と向きは、取得した状態を初期表示します。未取得の値は「未取得」と表示し、設定を変更するまで送信ボタンを無効にします。操作すると表示が送信用の設定値に変わり、「適用」を押すと送信されます。エアコンは未対応のため一覧に表示しません。

## Run

機器の操作結果はカード内に表示します。一覧更新では検索条件を保ち、詳細画面から戻ると検索中の機器と元のカードに戻れます。未送信の設定は同じタブ内に保存し、一覧更新や画面往復で復元します。送信に成功した設定は保存対象から外し、他の機器の未送信設定は保持します。

```sh
export SWITCHBOT_TOKEN='...'
export SWITCHBOT_SECRET='...'
export SWITCHBOT_PANEL_PASSWORD='choose-a-local-password'
go run .
```

または `switchbot-panel.env` を置いて起動できます。

```sh
cp switchbot-panel.env.example switchbot-panel.env
$EDITOR switchbot-panel.env
go run .
```

Open `http://<linux-server-lan-ip>:8080/`.

`bash deploy.sh minlnx1` で SSH 経由の配置とユーザーサービスの起動ができます。SSH 切断後・再起動後もサービスを動かすため、デプロイ時に linger を有効化します。権限不足の場合はサーバー上で `sudo loginctl enable-linger "$USER"` を実行してから再実行してください。

`SWITCHBOT_PANEL_PASSWORD` を設定すると Basic 認証が有効になります。ユーザー名は `admin` です。

## Environment

| Name | Default | Description |
| --- | --- | --- |
| `SWITCHBOT_TOKEN` | required | SwitchBot app の Open Token |
| `SWITCHBOT_SECRET` | required | SwitchBot app の Secret Key |
| `SWITCHBOT_PANEL_PASSWORD` | empty | Basic 認証パスワード |
| `SWITCHBOT_PANEL_ADDR` | `0.0.0.0:8080` | listen address |
| `SWITCHBOT_DEVICE_CACHE_TTL` | `10m` | デバイス一覧キャッシュ |
| `SWITCHBOT_STATUS_CACHE_TTL` | `20s` | ステータスキャッシュ |

## Supported Commands

現時点では事故を避けるため、UI から送れるコマンドはデバイス種別ごとの allowlist に絞っています。

- Bot: `press`, `turnOn`, `turnOff`
- Lock: `lock`, `unlock`
- Curtain / Roller Shade: `turnOn`, `turnOff`, `pause`, `setPosition`
- Blind Tilt: `fullyOpen`, `closeUp`, `closeDown`, `setPosition`
- Lighting: `turnOn`, `turnOff`, `toggle`, brightness, color temperature, RGB
- Plug / Relay Switch: `turnOn`, `turnOff`, `toggle`
- Infrared remotes: common power plus major TV, fan, and light operations
