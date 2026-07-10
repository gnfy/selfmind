# Weixin Live Test

This checklist verifies the real-device path that mocks cannot prove: iLink
acceptance, phone delivery, cross-endpoint identity, detached notifications,
approval resumption, media transfer, and stale-session catch-up.

## 0. Preflight

Run in WSL as the same Linux user that owns `~/.selfmind`:

```bash
selfmind id
selfmind weixin status
selfmind gateway status
sha256sum /mnt/d/wwwroot/ai/selfmind/testdata/weixin-live/outbound-file.txt
```

Pass when:

- `gateway` is running and has no unexpected active run;
- Weixin is enabled, its token is configured, and the credential file exists;
- `owner_person_id` is the same person reported by `selfmind id`;
- `sync_state` keeps updating after a WeChat message arrives.

If login or binding is missing:

```bash
selfmind weixin login --owner-person-id person_xxx --timeout 8m
selfmind gateway restart
```

Do not paste tokens, account files, or unredacted doctor output into chat.

## 1. Identity And Control Plane

Send these messages to the bot in a private WeChat chat:

```text
/id
/model
/status
/diag
```

Pass when `/id` matches the CLI person, every command returns once, and no
control command creates a new work task.

## 2. Synchronous Inbound And Final Reply

Send:

```text
请只回复：WX-TEXT-20260710-A1
```

Pass when WeChat shows a working/typing indication promptly, then receives one
complete final reply containing the marker. It must not stream token fragments
or deliver the final reply twice.

## 3. Detached CLI To Weixin

From WSL:

```bash
selfmind send "/notify weixin"
selfmind send --async "请只回复：WX-CLI-ASYNC-20260710-C1"
```

The second command should return immediately. Pass when its final result later
arrives in WeChat exactly once without keeping a CLI client open.

## 4. Approval Round Trip

In WeChat:

```text
/mode read-only
请执行命令 printf 'WX-APPROVAL-20260710-D1\n'，然后告诉我输出。
```

Reply `y` to the approval prompt. Pass when the same run resumes and returns
the marker. Repeat once and reply `n` to verify rejection, then restore:

```text
/mode on-request
```

## 5. Media

### Outbound file

Send:

```text
请只回复这一行，不要加代码块：MEDIA:/mnt/d/wwwroot/ai/selfmind/testdata/weixin-live/outbound-file.txt
```

Download the file on the phone. Pass when its first marker and Chinese line are
intact and its SHA-256 matches the preflight value.

### Inbound image/file

Send any phone photo and the generated text fixture back to the bot, with:

```text
请只告诉我附件的类型、文件名和 MIME，不需要识别图片内容。
```

Pass when SelfMind reports attachment metadata and the gateway log has no media
decrypt/download error.

### Voice

Send a voice message saying:

```text
微信语音测试，编号 WX-VOICE-20260710-E1
```

Pass when the transcript reaches the agent and the reply preserves the marker.

## 6. Delivery Health And Catch-Up

Send `/diag`. Record the `Outbound (24h)` counts. The normal pass state is zero
new `failed` rows and zero new `sent_unconfirmed` rows.

If a pushed result is accepted by iLink but does not arrive on the phone:

1. Record `/diag` and the approximate time.
2. Send one ordinary message in the same WeChat chat to refresh context token.
3. The unconfirmed result should be re-pushed once, not repeatedly.
4. Send `/diag` again and record the final counts.

## Result Sheet

| Case | Result | Notes |
| --- | --- | --- |
| Preflight | pending | |
| Identity/control | pending | |
| Inbound/final reply | pending | |
| Detached CLI -> Weixin | pending | |
| Approval approve/reject | pending | |
| Outbound file | pending | |
| Inbound image/file | pending | |
| Voice transcript | pending | |
| Delivery health/catch-up | pending | |

