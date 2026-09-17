// AudioWorklet 采集处理器。
//
// 必须作为独立文件由同源静态服务提供：Safari 不接受用 blob: URL 调
// audioWorklet.addModule()，会直接报 "Unable to load a worklet's module."。
//
// 职责很简单：把 128 采样一次的回调攒成大块再抛回主线程，减少 postMessage 次数。
// 降采样和 PCM16 转换都放在主线程做，这里只管收集。
class Collector extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buf = [];
    this.len = 0;
    this.chunk = 128 * 8; // 约 21ms @48k，够小保证实时性，够大减少消息数
  }

  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (ch) {
      this.buf.push(new Float32Array(ch));
      this.len += ch.length;
      if (this.len >= this.chunk) {
        const merged = new Float32Array(this.len);
        let off = 0;
        for (const b of this.buf) {
          merged.set(b, off);
          off += b.length;
        }
        this.port.postMessage(merged, [merged.buffer]);
        this.buf = [];
        this.len = 0;
      }
    }
    return true;
  }
}

registerProcessor('collector', Collector);
