package dashscope

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/asr"
)

// TestRecognizePCMFile 用本地 PCM 文件跑通上游协议，不涉及 HTTP 层。
//
// 需要真实凭据，按需设置环境变量后运行：
//
//	ASR_API_KEY=sk-xxx ASR_WORKSPACE_ID=llm-xxx ASR_PCM_FILE=/path/16k.pcm \
//	go test ./pkg/llm/asr/dashscope/ -run TestRecognizePCMFile -v
//
// PCM 要求：16kHz、单声道、16bit 小端裸流。
func TestRecognizePCMFile(t *testing.T) {
	apiKey := ""
	workspaceID := os.Getenv("ASR_WORKSPACE_ID")
	pcmFile := "./16k16bit.pcm"
	if apiKey == "" || pcmFile == "" {
		t.Skip("缺少 ASR_API_KEY / ASR_WORKSPACE_ID / ASR_PCM_FILE，跳过")
	}

	pcm, err := os.ReadFile(pcmFile)
	if err != nil {
		t.Fatalf("读取 pcm 失败: %v", err)
	}

	region := os.Getenv("ASR_REGION")
	if region == "" {
		region = "cn-beijing"
	}

	r := New(Config{
		Region:      region,
		WorkspaceID: workspaceID,
		APIKey:      apiKey,
		Model:       "qwen-audio-3.0-asr-flash-streaming",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sess, err := r.Start(ctx, asr.RecognizeOption{
		Format:              "pcm",
		SampleRate:          16000,
		LanguageHints:       []string{"zh"},
		SemanticPunctuation: true,
		RequestID:           "test-" + time.Now().Format("150405"),
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer sess.Close()

	// 按 100ms 一帧（16000 * 2byte * 0.1s = 3200 字节）实时节奏灌入。
	const frameSize = 3200
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for off := 0; off < len(pcm); off += frameSize {
			end := off + frameSize
			if end > len(pcm) {
				end = len(pcm)
			}
			<-ticker.C
			if err := sess.Write(pcm[off:end]); err != nil {
				t.Logf("写音频中断: %v", err)
				return
			}
		}
		if err := sess.Finish(); err != nil {
			t.Logf("finish-task 失败: %v", err)
		}
	}()

	var finals []string
	for event := range sess.Events.EventCh {
		switch event.Type {
		case asr.EventASRReady:
			t.Logf("[ready] session=%s", event.SessionID)
		case asr.EventASRPartial:
			t.Logf("[partial] #%d %s", event.SentenceID, event.Text)
		case asr.EventASRFinal:
			t.Logf("[final]   #%d %s (%dms-%dms)", event.SentenceID, event.Text, event.BeginMs, event.EndMs)
			finals = append(finals, event.Text)
		case asr.EventASRFinished:
			t.Logf("[finished] 计费时长 %ds", event.DurationSec)
		case asr.EventASRFailed:
			t.Fatalf("[failed] code=%s err=%v", event.Code, event.Err)
		}
	}

	if len(finals) == 0 {
		t.Fatal("没有拿到任何定稿句子")
	}
}
