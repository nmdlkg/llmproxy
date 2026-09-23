# CLIProxyAPI 사용자 가이드

버전: v0.3
작성일: 2026-08-16

이 문서는 공유형 CLIProxyAPI의 일반 사용자용 안내서입니다.
사용자 생성과 전역 model catalog 변경은
[docs/admin-level-runbook.md](docs/admin-level-runbook.md)를 참고하세요.

프록시는 OpenAI, Claude, Gemini, Codex 호환 API를 제공하며 여러 사용자가 공유하는
AI 서비스 계정 묶음을 통해 요청을 처리합니다. proxy 주소는 100.110.30.57입니다.

## 연결 주소와 보안 범위

현재 proxy의 8317 포트는 사설 오버레이 네트워크에서 HTTP로 서비스됩니다.
따라서 이 문서의 API·웹 주소는 `http://100.110.30.57:8317`을 사용합니다.
이 주소는 신뢰된 사설 네트워크에서만 사용하고, 공용 네트워크나 인터넷에 노출하지
마세요. TLS(HTTPS)가 필요한 환경에서는 관리자가 proxy에 유효한 HTTPS 종단을
구성해야 합니다.

일반적인 사용 프로그램을 연결할 때 이 레포지토리나 cli-proxy-api 실행 파일을
설치할 필요가 없습니다. proxy 접속 주소와 사용자 API key만 있으면 됩니다.

## 0. 이 서비스의 역할과 사용 정책

CLIProxyAPI는 사용자의 프로그램(chatgpt app 등)과 여러 AI 서비스 사이를 연결하는 중계 서비스입니다.
사용자는 이 서버에 요청을 보내고, 서버는 공유용으로 등록된 여러 AI 서비스 계정 중 가용 가능한
계정을 적절히 골라 대신 요청을 전달하여 여러 계정을 끊김없이 사용할 수 있습니다. 사용자의 AI 서비스 로그인 정보는 서버에서
관리합니다.

공용 계정 공유 정책은 다음과 같습니다.

- 사용자가 등록한 AI 서비스 로그인 정보는 처음에는 본인만 사용할 수 있습니다.
- 공유를 켜면 해당 계정이 모든 사용자가 함께 쓰는 공용 계정 묶음에 들어갑니다.
- 공유에 참여하면 본인이 제공한 계정의 종류와 요금제에 따라 주간 사용 한도가
  늘어납니다. 다른 사용자는 그 계정을 사용할 수 있지만, 계정 소유자의 로그인
  정보 자체를 볼 수는 없습니다.
- 주간 사용 한도는 실제 결제 금액이 아니라 요청량을 비교하기 위한 USD 환산
  기준입니다. 기본 제공 한도와 공유한 계정의 기여분을 합산해 계산합니다.

사용 한도와 공유 여부는 /user 웹 화면에서 확인할 수 있습니다. 운영 정책에 따라
한도 초과 시 요청이 제한되거나 더 저렴한 모델로 전환될 수 있습니다.

## 1. 사용자 API 키와 웹 화면

관리자가 cp_u_... 형식의 키를 전달합니다. 키는 발급 시 한 번만 평문으로
표시되고 서버에는 hash만 저장됩니다. 잃어버리면 관리자에게 새 키를 요청하거나
로그인 후 /user에서 새 키를 발급하세요.

~~~text
http://100.110.30.57:8317/user
~~~

사용자 키로 자신의 AI 서비스 로그인 정보 등록·조회·삭제, 공용 계정 묶음 참여
여부 변경, 주간 사용 한도 확인, 장치별 사용자 키 발급·폐기를 할 수 있습니다.
다른 사용자의 로그인 정보는 볼 수 없습니다. 새로 등록한 로그인 정보는 기본적으로
공유되지 않습니다.

## 2. 선택 사항: AI 서비스 계정 등록

공용 계정 묶음에 본인의 AI 서비스 계정을 보태려는 경우에만 이 절을 진행합니다.
가장 간단한 방법은 proxy 웹 화면에서 사용자 키로 AI 서비스 로그인을 시작하는
것입니다. 별도의 레포지토리 복사나 서버 설치는 필요하지 않습니다.

~~~text
http://100.110.30.57:8317/user
~~~

### [2.Optional] CLI login

브라우저 로그인 완료 신호를 자신의 컴퓨터에서 처리해야 하거나 웹 화면 대신
명령줄 방식을 사용하려는 경우에만 CLI login을 사용합니다. 이 명령은 proxy 접속
주소를 사용하기 위한 필수 설치가 아닙니다. 실행 파일이 이미 제공된 경우에만
사용하고, 이 레포를 사용자 컴퓨터에 복사하거나 빌드할 필요는 없습니다.

현재 proxy는 HTTPS를 제공하지 않으므로 이 방식은 사용할 수 없습니다. CLI login의
원격 endpoint는 loopback이 아닌 경우 HTTPS가 필요하므로, `--remote`에 HTTP 주소를
넣지 마세요. AI 서비스 계정을 등록하려면 이 문서의 `/user` 웹 화면을 사용하세요.
관리자가 HTTPS 종단을 구성한 뒤에만 아래 형식으로 CLI login을 사용할 수 있습니다.

~~~text
cli-proxy-api[.exe] --claude-login --remote https://<관리자가 제공한 HTTPS 주소> --user-key cp_u_...
~~~

| Provider | Flag |
| --- | --- |
| Claude | --claude-login |
| Codex browser | --codex-login |
| Codex device code | --codex-device-login |
| Antigravity | --antigravity-login |
| Kimi | --kimi-login |
| xAI | --xai-login |

브라우저를 열 수 없으면 --no-browser, 로그인 완료를 받을 port가 사용 중이면
--oauth-callback-port <port>를 사용하세요.

웹 화면 로그인에서 localhost 이동 실패가 보이는 것은 정상입니다. 주소창의
전체 URL을 복사해 웹 화면에 다시 입력하세요. 이미 AI 서비스 계정이 관리자에
의해 등록되었거나 계정 공유가 필요하지 않으면 이 절을 건너뛰면 됩니다.

## 3. 기본 설정: Codex (ChatGPT desktop app)

ChatGPT desktop app을 기본 사용 프로그램으로 연결하는 설정입니다. Codex 명령줄
도구와 IDE 확장 기능도 같은 사용자 설정을 읽지만, 이 가이드의 기본 경로는
desktop app입니다.

파일 위치:

- Linux/macOS: ~/.codex/config.toml
- Windows: %USERPROFILE%\.codex\config.toml

ChatGPT desktop에서는 Settings > Configuration > Open config.toml을 사용합니다.
이 설정은 컴퓨터에서 실행되는 Codex 작업에만 적용되고 일반 ChatGPT 온라인
대화의 연결 경로는 바꾸지 않습니다. 기존 model과 model_provider가 있으면 중복
key를 추가하지 말고 수정하세요. 프로젝트 폴더 안의 .codex/config.toml이 아니라
사용자 설정 파일에 provider를 둡니다.

~~~toml
model_provider = "nmdl"

[model_providers.nmdl]
name = "CLIProxyAPI"
base_url = "http://100.110.30.57:8317/v1"
wire_api = "responses"
~~~

provider ID를 openai로 지정하거나 requires_openai_auth = true를 사용하지 마세요.
proxy는 ChatGPT 로그인 정보가 아니라 cp_u_... 사용자 키를 사용합니다.
### 4.a Windows: 운영체제 기본 암호화 사용

Windows DPAPI는 현재 Windows 사용자와 컴퓨터에 연결해 키를 암호화하는
Windows 기본 기능입니다.

~~~powershell
$secureKey = Read-Host 'CLIProxy per-user key' -AsSecureString
$secretDirectory = Join-Path $env:APPDATA 'CLIProxyAPI'
$secretPath = Join-Path $secretDirectory 'api-key.dpapi'
New-Item -ItemType Directory -Force -Path $secretDirectory | Out-Null
$encryptedKey = ConvertFrom-SecureString $secureKey
[IO.File]::WriteAllText($secretPath, $encryptedKey)
$encryptedKey = $null
$secureKey = $null
~~~

%USERPROFILE%\.codex\get-cliproxy-key.ps1:

~~~powershell
$ErrorActionPreference = 'Stop'
$secretPath = Join-Path $env:APPDATA 'CLIProxyAPI\api-key.dpapi'
$encryptedKey = [IO.File]::ReadAllText($secretPath).Trim()
$secureKey = ConvertTo-SecureString $encryptedKey
$keyPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
try {
    $plainKey = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($keyPointer)
    [Console]::Out.Write($plainKey)
} finally {
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($keyPointer)
    $plainKey = $null
    $secureKey = $null
}
~~~

검증:

~~~powershell
$retrievedKey = & "$HOME\.codex\get-cliproxy-key.ps1"
if ($retrievedKey) { "key retrieval succeeded (length: $($retrievedKey.Length))" }
$retrievedKey = $null
~~~

%USERPROFILE%\.codex\config.toml에 추가:

~~~toml
[model_providers.nmdl.auth]
command = "powershell.exe"
args = [
  "-NoLogo",
  "-NoProfile",
  "-NonInteractive",
  "-ExecutionPolicy",
  "RemoteSigned",
  "-Command",
  '& "$env:USERPROFILE\.codex\get-cliproxy-key.ps1"',
]
timeout_ms = 5000
refresh_interval_ms = 0
~~~

위 로그인 키 자동 조회 설정을 사용하면 env_key를 함께 설정하지 마세요. desktop
설정 변경 후 백그라운드에서 실행 중인 프로그램까지 완전히 종료하고 다시 시작합니다.


### 4.b Linux/macOS: 로그인 키 보관함 사용

Ubuntu의 Secret Service는 로그인 키를 운영체제의 보안 보관함에 저장하는
기능입니다.

~~~bash
sudo apt update
sudo apt install libsecret-tools
secret-tool store --label='CLIProxy API key' service cliproxy provider nmdl
secret-tool lookup service cliproxy provider nmdl >/dev/null
~~~

~~~toml
[model_providers.nmdl.auth]
command = "/usr/bin/secret-tool"
args = ["lookup", "service", "cliproxy", "provider", "nmdl"]
timeout_ms = 5000
refresh_interval_ms = 0
~~~

위 로그인 키 자동 조회 설정을 사용하면 env_key를 함께 설정하지 마세요.


### [4.b Optional] Linux/macOS: 한 번만 쓰는 명령창과 프로젝트별 설정

~~~toml
[model_providers.nmdl]
env_key = "CLIPROXY_API_KEY"
~~~

~~~bash
read -rsp 'CLIProxy per-user key: ' CLIPROXY_API_KEY
echo
export CLIPROXY_API_KEY
codex exec -m gpt-5.6-sol 'Reply exactly: nmdl-codex-ok'
~~~

프로젝트별로만 키를 사용하려면 direnv라는 환경 관리 도구를 사용할 수 있습니다.
~/.codex/config.toml에는 env_key만 두고 auth table은 넣지 않습니다.

~~~bash
eval "$(direnv hook bash)"
export CLIPROXY_API_KEY="$(secret-tool lookup service cliproxy provider nmdl)"
cd /path/to/project
direnv allow .
~~~

평문 키를 .envrc에 넣지 말고 사용자 홈 폴더에서 .envrc를 허용하지 마세요.

## 선택 사항: Anthropic과 Claude Code

Claude Code를 proxy에 연결할 때만 이 절을 진행합니다. Codex와 ChatGPT
desktop만 사용하는 경우에는 건너뜁니다. 먼저 이 문서의 provider 계정 등록
절에서 Claude 로그인 정보를 등록해야 합니다.

### 기본 설정: `~/.claude/settings.json`

셸의 `export`만 사용하면 기존 터미널, IDE, 백그라운드 세션에 설정이 빠질 수
있습니다. 기본 설정으로 `~/.claude/settings.json`을 사용하세요. 키를 JSON에
직접 쓰지 않고 권한이 `0600`인 키 파일에서 읽습니다. 기존 설정 파일이 있으면
아래 key만 병합하고, 다른 설정을 덮어쓰지 마세요.

Linux/macOS에서 먼저 키 파일을 만듭니다.

~~~bash
mkdir -p ~/.config/opencode
read -rsp 'CLIProxy per-user key: ' CLIPROXY_KEY
echo
printf '%s' "$CLIPROXY_KEY" > ~/.config/opencode/cliproxy-user-key
chmod 600 ~/.config/opencode/cliproxy-user-key
unset CLIPROXY_KEY
~~~

그다음 `~/.claude/settings.json`에 추가합니다.

~~~json
{
  "apiKeyHelper": "tr -d '\\r\\n' < ~/.config/opencode/cliproxy-user-key",
  "env": {
    "ANTHROPIC_BASE_URL": "http://100.110.30.57:8317"
  }
}
~~~

`apiKeyHelper`는 키 파일의 줄바꿈을 제거해 API key로 출력합니다. 이 파일과
`settings.json`에는 `cp_u_...` 평문 키를 넣지 마세요.

### 일회성 셸 설정

임시로 실행한 명령창에서만 설정을 적용하려면 아래 환경변수를 사용합니다.

Linux/macOS:

~~~bash
export ANTHROPIC_BASE_URL=http://100.110.30.57:8317
export ANTHROPIC_API_KEY=cp_u_...
export OTEL_RESOURCE_ATTRIBUTES=llm_route=proxy
~~~

Windows PowerShell:

~~~powershell
$env:ANTHROPIC_BASE_URL = 'http://100.110.30.57:8317'
$env:ANTHROPIC_API_KEY = 'cp_u_...'
$env:OTEL_RESOURCE_ATTRIBUTES = 'llm_route=proxy'
~~~

Claude Code를 이 환경이 설정된 명령창에서 시작합니다. 일반 프로그램을 직접
연결하는 경우에도 같은 Anthropic 접속 주소와 사용자 키를 사용할 수 있습니다.

`OTEL_RESOURCE_ATTRIBUTES=llm_route=proxy`는 telemetry 활성화 옵션이 아니라
telemetry에 붙는 단순 resource attribute 태그입니다. telemetry opt-out이나
비활성화 기능으로 해석하면 안 됩니다.

### 검증과 별도 통신

~~~bash
claude -p "Reply exactly: claude-proxy-ok" --model sonnet
~~~

interactive Claude에서는 `/status`를 실행해 `Anthropic base URL`이
`http://100.110.30.57:8317`인지 확인하세요.

모델 요청은 gateway로 보내더라도 Claude Code는 fast-mode availability check,
WebFetch safety check, telemetry 등에서 별도의 HTTPS 통신을 할 수 있습니다.
필요할 때만 아래 선택 사항을 적용하세요.

~~~bash
# 비필수 외부 통신 전체 차단: auto-update, fast-mode availability check,
# feature flags, Remote Control 등도 비활성화됩니다.
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# telemetry만 차단
export DISABLE_TELEMETRY=1

# 오류 보고 차단
export DISABLE_ERROR_REPORTING=1
~~~

WebFetch의 `api.anthropic.com` safety check도 차단해야 하는 경우에는
`~/.claude/settings.json`에 다음 값을 추가할 수 있습니다. 이 값은 WebFetch
사전 점검을 건너뛰므로 그 영향을 이해한 경우에만 사용하세요.

~~~json
{
  "skipWebFetchPreflight": true
}
~~~

## Linux CLI 전용 OpenCode

이 절은 Linux 명령줄에서 OpenCode를 사용할 때만 해당합니다. Windows OpenCode나
화면 기반 프로그램의 설정 방법이 아닙니다.

~~~bash
opencode --version
mkdir -p ~/.config/opencode
if test -f ~/.config/opencode/opencode.jsonc; then
  cp -a ~/.config/opencode/opencode.jsonc \
    ~/.config/opencode/opencode.jsonc.before-cliproxy
elif test -f ~/.config/opencode/opencode.json; then
  cp -a ~/.config/opencode/opencode.json \
    ~/.config/opencode/opencode.json.before-cliproxy
fi

# Reuse the per-user key issued by CLIProxyAPI.
test -s ~/.config/cliproxy/api-key || {
  mkdir -p ~/.config/cliproxy
  read -rsp 'CLIProxy per-user key: ' CLIPROXY_KEY
  echo
  printf '%s' "$CLIPROXY_KEY" > ~/.config/cliproxy/api-key
  chmod 600 ~/.config/cliproxy/api-key
  unset CLIPROXY_KEY
}
~~~

OpenCode 설정 파일(`~/.config/opencode/opencode.json` 또는
`~/.config/opencode/opencode.jsonc`)의 기존 `provider` 객체에 `cliproxy`를
병합합니다. 기존 provider를 통째로 덮어쓰지 마세요. OpenCode의 현재 설정 문법은
`provider.<id>.models`에 모델을 등록하고 `options.baseURL`과 `options.apiKey`를
지정하는 방식입니다. `/v1/models`에 모델이 보여도 OpenCode의 선택 목록에 추가하지
않으면 사용할 수 없습니다. 문법의 기준은 [OpenCode Config 문서](https://opencode.ai/docs/config/)입니다.

~~~jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "cliproxy": {
      "name": "CLIProxyAPI",
      "models": {
        "gpt-5.6-sol": {
          "name": "GPT 5.6 Sol via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "gpt-5.6-sol-fast": {
          "name": "GPT 5.6 Sol Fast via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "gpt-5.6-terra": {
          "name": "GPT 5.6 Terra via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "gpt-5.6-terra-fast": {
          "name": "GPT 5.6 Terra Fast via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "gpt-5.6-luna": {
          "name": "GPT 5.6 Luna via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "gpt-5.6-luna-fast": {
          "name": "GPT 5.6 Luna Fast via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "claude-opus-5": {
          "name": "Claude Opus 5 via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "auto": { "reasoningEffort": "auto" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "claude-fable-5": {
          "name": "Claude Fable 5 via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "auto": { "reasoningEffort": "auto" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "claude-sonnet-5": {
          "name": "Claude Sonnet 5 via CLIProxyAPI",
          "variants": {
            "none": { "reasoningEffort": "none" },
            "auto": { "reasoningEffort": "auto" },
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" },
            "xhigh": { "reasoningEffort": "xhigh" },
            "max": { "reasoningEffort": "max" }
          }
        }
      },
      "options": {
        "baseURL": "http://100.110.30.57:8317/v1",
        "apiKey": "{file:~/.config/cliproxy/api-key}"
      }
    }
  }
}
~~~

`gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`의 fast 항목은 OpenCode에서
별도 모델로 선택되는 client-visible alias입니다. fast 요청을 실제 priority tier로
보내려면 proxy `config.yaml`에도 관리자가 다음 두 블록을 추가해야 합니다.

~~~yaml
oauth-model-alias:
  codex:
    - name: "gpt-5.6-sol"
      alias: "gpt-5.6-sol-fast"
      fork: true
    - name: "gpt-5.6-terra"
      alias: "gpt-5.6-terra-fast"
      fork: true
    - name: "gpt-5.6-luna"
      alias: "gpt-5.6-luna-fast"
      fork: true

payload:
  override:
    - models:
        - name: "gpt-5.6-sol-fast"
          protocol: "codex"
        - name: "gpt-5.6-terra-fast"
          protocol: "codex"
        - name: "gpt-5.6-luna-fast"
          protocol: "codex"
      params:
        service_tier: priority
~~~

관리자는 이 변경을 적용한 뒤 proxy를 재시작하거나 설정 hot-reload가 완료된 것을
확인해야 합니다. OpenCode의 fast 항목만 추가하면 모델 선택 목록에는 나타나지만
upstream priority tier가 자동으로 활성화되지는 않습니다.

기존 `provider` 또는 `cliproxy` 설정을 중복 생성하지 말고, 실제 proxy가 해당
모델을 제공하는지 먼저 확인하세요. Claude 모델의 정확한 client-visible ID는
`claude-opus-5`, `claude-fable-5`, `claude-sonnet-5`입니다.

Anthropic 공식 Effort 단계는 세 모델 모두 `low`, `medium`, `high`, `xhigh`,
`max`입니다. `none`은 CLIProxyAPI가 thinking을 끄는 proxy 제어이고, `auto`는
Anthropic adaptive thinking의 기본 선택에 맡기는 proxy 제어이므로 공식 Effort
단계와 구분하세요. OpenCode에서는 모델 실행 시 `--variant`로 선택합니다.

~~~bash
opencode run -m cliproxy/claude-opus-5 --variant low 'Reply exactly: opus-low-ok'
opencode run -m cliproxy/claude-fable-5 --variant xhigh 'Reply exactly: fable-xhigh-ok'
opencode run -m cliproxy/claude-sonnet-5 --variant max 'Reply exactly: sonnet-max-ok'
~~~

근거: [Anthropic Effort 문서](https://platform.claude.com/docs/en/build-with-claude/effort),
CLIProxyAPI의 Claude thinking 레지스트리와 변환기입니다. `xhigh`와 `max`는 모델별
지원 범위가 다를 수 있으므로 이 설정에서는 현재 세 모델의 catalog 선언에 맞췄습니다.

~~~bash
export OTEL_RESOURCE_ATTRIBUTES=llm_route=proxy
curl -fsS http://100.110.30.57:8317/healthz
opencode models cliproxy | rg '^cliproxy/(claude-(opus|fable|sonnet)-5|gpt-5\.6-(sol|terra|luna)(-fast)?)$'
opencode run -m cliproxy/gpt-5.6-sol 'Reply exactly: opencode-proxy-ok'
opencode run -m cliproxy/gpt-5.6-sol-fast 'Reply exactly: opencode-proxy-fast-ok'
~~~

기존에 컴퓨터에서 별도로 실행하던 CLIProxyAPI와 충돌하지 않는지 확인합니다.

~~~bash
systemctl --user is-enabled cliproxyapi.service 2>/dev/null || true
systemctl --user is-active cliproxyapi.service 2>/dev/null || true
ss -ltnp | rg '127\.0\.0\.1:8317|:8317' || true
pgrep -af 'cliproxyapi|cli-proxy-api' || true
~~~

OpenCode가 실제 proxy 주소를 가리키는지 확인하세요. auth.json에 cliproxy
로그인 정보가 있으면 options.apiKey보다 먼저 사용될 수 있으므로 값은 출력하지
말고 provider 이름만 확인합니다.

~~~bash
if test -f ~/.local/share/opencode/auth.json; then
  jq -r 'keys[]' ~/.local/share/opencode/auth.json | sort
fi
~~~

## 6. 주간 사용 한도와 문제 해결

주간 사용 한도는 최근 7일을 기준으로 계산되는 USD 환산 사용량이며 실제 청구액이
아닙니다. auto를 모델 이름으로 요청하면 prompt 난이도에 따라 proxy가 선택합니다.
auto(high)
같은 thinking suffix도 사용할 수 있습니다.

~~~bash
curl -H 'Authorization: Bearer cp_u_...' \
  http://100.110.30.57:8317/v0/user/usage
~~~

| 증상 | 확인할 내용 |
| --- | --- |
| 401 또는 auth error | 키가 틀렸거나 폐기되었거나 계정이 중지되었는지 확인합니다. |
| 429 및 Retry-After | 주간 사용 한도 또는 전체 공용 계정 묶음의 일시적 제한입니다. |
| /user가 열리지 않음 | 사용자 기능이 꺼져 있거나 사내 네트워크 연결이 없는지 확인합니다. |
| OAuth 로그인 실패 | 실패한 localhost URL 전체를 웹 화면에 다시 입력하거나 CLI login을 사용합니다. |
| 로그인 정보 오류 | AI 서비스 로그인 정보가 만료되었을 수 있으므로 로그인을 다시 실행합니다. |
| Codex/OpenCode 모델 누락 | 서버의 모델 목록과 사용 프로그램의 선택 목록을 각각 확인합니다. |

키가 노출되었다고 의심되면 즉시 폐기하고 /user에서 새 장치별 키를 발급하세요.
