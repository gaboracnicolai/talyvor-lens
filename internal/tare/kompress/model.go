// Package kompress is Tare phase 2a (B27.35): the Apache-2.0 kompress-small token classifier
// (huggingface.co/chopratejas/kompress-small — a 6-layer ModernBERT distilled from kompress-base)
// run in pure Go, behind the phase-1 Reduction interface.
//
// ⚠ WHY PURE GO AND NOT ONNX RUNTIME. The lens binary is built CGO_ENABLED=0 and ships on alpine
// (musl). onnxruntime is a C++ library: every Go binding to it needs cgo or a glibc dlopen, so the
// published model.onnx cannot be executed in this image as published. Its graph is plain ModernBERT
// (MatMul, LayerNormalization, Softmax, Erf-GELU, RoPE, a ±64-token sliding window on the local
// layers), so this file computes that graph directly from the same weights (model.safetensors, the
// tensors the ONNX export was traced from). golden_test.go holds logits produced by onnxruntime on
// the published model.onnx and asserts this forward pass reproduces them.
package kompress

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sync"
)

// The shape of kompress-small, read off its ONNX graph and safetensors header.
const (
	hidden       = 768
	heads        = 12
	headDim      = hidden / heads
	intermediate = 1152
	numLayers    = 6
	vocabSize    = 50368
	normEps      = 1e-5
	localWindow  = 64 // |i-j| <= 64: ModernBERT's local_attention=128, as LessOrEqual(Abs(i-j), 64) in the graph
	globalTheta  = 160000.0
	localTheta   = 10000.0
)

// globalLayer says which layers attend globally. The student kept teacher layers [0,4,8,12,17,21]
// and with them the teacher's every-third-layer-is-global pattern (index%3 == 0): 0, 12 and 21,
// which are student layers 0, 3 and 5. Read from the graph: those three Softmaxes add the padding
// mask, the other three add the sliding-window mask, and the RoPE theta follows the same split.
var globalLayer = [numLayers]bool{true, false, false, true, false, true}

type layer struct {
	attnNorm []float32 // nil on layer 0: ModernBERT's first layer has no attention norm
	wqkv     []float32 // [3*hidden, hidden]
	wo       []float32 // [hidden, hidden]
	mlpNorm  []float32
	wi       []float32 // [2*intermediate, hidden]: input half then gate half
	wo2      []float32 // [hidden, intermediate]
}

// Model is kompress-small's encoder and token head. Safe for concurrent use: Logits allocates its
// own activations and only reads the weights.
type Model struct {
	emb, embNorm, finalNorm []float32
	layers                  [numLayers]layer
	headW, headB            []float32 // [2, hidden], [2]
	ropeGlobal, ropeLocal   []float64 // inverse frequencies, headDim/2 each
}

// LoadModel reads kompress-small's model.safetensors.
func LoadModel(path string) (*Model, error) {
	t, err := readSafetensors(path)
	if err != nil {
		return nil, err
	}
	get := func(name string, shape ...int) ([]float32, error) {
		v, ok := t[name]
		if !ok {
			return nil, fmt.Errorf("kompress: %s: tensor %q missing", path, name)
		}
		n := 1
		for _, d := range shape {
			n *= d
		}
		if len(v) != n {
			return nil, fmt.Errorf("kompress: %s: tensor %q has %d values, want %d", path, name, len(v), n)
		}
		return v, nil
	}
	m := &Model{}
	var errs []error
	must := func(dst *[]float32, name string, shape ...int) {
		v, err := get(name, shape...)
		if err != nil {
			errs = append(errs, err)
		}
		*dst = v
	}
	must(&m.emb, "encoder.embeddings.tok_embeddings.weight", vocabSize, hidden)
	must(&m.embNorm, "encoder.embeddings.norm.weight", hidden)
	must(&m.finalNorm, "encoder.final_norm.weight", hidden)
	must(&m.headW, "token_head.weight", 2, hidden)
	must(&m.headB, "token_head.bias", 2)
	for i := range m.layers {
		p := fmt.Sprintf("encoder.layers.%d.", i)
		l := &m.layers[i]
		if i > 0 {
			must(&l.attnNorm, p+"attn_norm.weight", hidden)
		}
		must(&l.wqkv, p+"attn.Wqkv.weight", 3*hidden, hidden)
		must(&l.wo, p+"attn.Wo.weight", hidden, hidden)
		must(&l.mlpNorm, p+"mlp_norm.weight", hidden)
		must(&l.wi, p+"mlp.Wi.weight", 2*intermediate, hidden)
		must(&l.wo2, p+"mlp.Wo.weight", hidden, intermediate)
	}
	if len(errs) > 0 {
		return nil, errs[0]
	}
	m.ropeGlobal = invFreq(globalTheta)
	m.ropeLocal = invFreq(localTheta)
	return m, nil
}

func invFreq(theta float64) []float64 {
	f := make([]float64, headDim/2)
	for i := range f {
		f[i] = 1 / math.Pow(theta, float64(2*i)/headDim)
	}
	return f
}

// readSafetensors loads every F32 tensor in a .safetensors file. The format is an 8-byte
// little-endian header length, a JSON header naming each tensor's dtype, shape and byte range, then
// the raw little-endian data.
func readSafetensors(path string) (map[string][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("kompress: %s: header length: %w", path, err)
	}
	if n > 64<<20 {
		return nil, fmt.Errorf("kompress: %s: header length %d is not plausible", path, n)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("kompress: %s: header: %w", path, err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &meta); err != nil {
		return nil, fmt.Errorf("kompress: %s: header: %w", path, err)
	}
	base := int64(8 + n)
	out := make(map[string][]float32, len(meta))
	buf := make([]byte, 1<<20)
	for name, raw := range meta {
		if name == "__metadata__" {
			continue
		}
		var info struct {
			DType   string   `json:"dtype"`
			Offsets [2]int64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil, fmt.Errorf("kompress: %s: tensor %q: %w", path, name, err)
		}
		if info.DType != "F32" {
			return nil, fmt.Errorf("kompress: %s: tensor %q is %s, only F32 is supported", path, name, info.DType)
		}
		size := info.Offsets[1] - info.Offsets[0]
		if size < 0 || size%4 != 0 {
			return nil, fmt.Errorf("kompress: %s: tensor %q has a bad byte range", path, name)
		}
		vals := make([]float32, size/4)
		for done := int64(0); done < size; {
			chunk := min(int64(len(buf)), size-done)
			if _, err := f.ReadAt(buf[:chunk], base+info.Offsets[0]+done); err != nil {
				return nil, fmt.Errorf("kompress: %s: tensor %q: %w", path, name, err)
			}
			for i := int64(0); i < chunk; i += 4 {
				vals[(done+i)/4] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i:]))
			}
			done += chunk
		}
		out[name] = vals
	}
	return out, nil
}

// Logits runs the encoder over one sequence of token ids (already wrapped in [CLS] … [SEP]) and
// returns [len(ids)][2] token logits; class 1 is "keep".
func (m *Model) Logits(ids []int) [][2]float32 {
	T := len(ids)
	h := make([]float32, T*hidden)
	for t, id := range ids {
		if id < 0 || id >= vocabSize {
			id = unkID
		}
		copy(h[t*hidden:(t+1)*hidden], m.emb[id*hidden:(id+1)*hidden])
	}
	layerNorm(h, h, m.embNorm)

	x := make([]float32, T*hidden)
	qkv := make([]float32, T*3*hidden)
	attn := make([]float32, T*hidden)
	proj := make([]float32, T*hidden)
	up := make([]float32, T*2*intermediate)
	act := make([]float32, T*intermediate)
	for li := range m.layers {
		l := &m.layers[li]
		in := h
		if l.attnNorm != nil {
			layerNorm(x, h, l.attnNorm)
			in = x
		}
		matmulT(qkv, in, l.wqkv, T, hidden, 3*hidden)
		freqs, window := m.ropeLocal, localWindow
		if globalLayer[li] {
			freqs, window = m.ropeGlobal, T
		}
		attention(attn, qkv, T, freqs, window)
		matmulT(proj, attn, l.wo, T, hidden, hidden)
		for i := range h {
			h[i] += proj[i]
		}

		layerNorm(x, h, l.mlpNorm)
		matmulT(up, x, l.wi, T, hidden, 2*intermediate)
		for t := 0; t < T; t++ {
			u := up[t*2*intermediate:]
			a := act[t*intermediate : (t+1)*intermediate]
			for j := range a {
				a[j] = gelu(u[j]) * u[intermediate+j]
			}
		}
		matmulT(proj, act, l.wo2, T, intermediate, hidden)
		for i := range h {
			h[i] += proj[i]
		}
	}
	layerNorm(h, h, m.finalNorm)

	out := make([][2]float32, T)
	for t := 0; t < T; t++ {
		row := h[t*hidden : (t+1)*hidden]
		out[t][0] = dot(row, m.headW[:hidden]) + m.headB[0]
		out[t][1] = dot(row, m.headW[hidden:]) + m.headB[1]
	}
	return out
}

// layerNorm writes w ⊙ (x−mean)/sqrt(var+eps) row by row. No bias: ModernBERT's norm_bias is false.
// dst may alias src.
func layerNorm(dst, src, w []float32) {
	n := len(w)
	for off := 0; off < len(src); off += n {
		row := src[off : off+n]
		var mean float64
		for _, v := range row {
			mean += float64(v)
		}
		mean /= float64(n)
		var variance float64
		for _, v := range row {
			d := float64(v) - mean
			variance += d * d
		}
		inv := 1 / math.Sqrt(variance/float64(n)+normEps)
		out := dst[off : off+n]
		for i, v := range row {
			out[i] = float32((float64(v)-mean)*inv) * w[i]
		}
	}
}

func gelu(x float32) float32 {
	return float32(0.5 * float64(x) * (1 + math.Erf(float64(x)/math.Sqrt2)))
}

// attention computes multi-head self-attention from a packed [T, 3, heads, headDim] qkv buffer
// into dst [T, hidden], with rotary position embeddings and |i-j| <= window masking.
func attention(dst, qkv []float32, T int, freqs []float64, window int) {
	// Rotate q and k in place, once per position.
	cos := make([]float32, T*headDim/2)
	sin := make([]float32, T*headDim/2)
	for p := 0; p < T; p++ {
		for i, f := range freqs {
			a := float64(p) * f
			cos[p*headDim/2+i] = float32(math.Cos(a))
			sin[p*headDim/2+i] = float32(math.Sin(a))
		}
	}
	half := headDim / 2
	for t := 0; t < T; t++ {
		c, s := cos[t*half:(t+1)*half], sin[t*half:(t+1)*half]
		for part := 0; part < 2; part++ { // q, then k
			for hd := 0; hd < heads; hd++ {
				v := qkv[t*3*hidden+part*hidden+hd*headDim:]
				for i := 0; i < half; i++ {
					x1, x2 := v[i], v[i+half]
					v[i] = x1*c[i] - x2*s[i]
					v[i+half] = x2*c[i] + x1*s[i]
				}
			}
		}
	}
	scale := float32(1 / math.Sqrt(headDim))
	parallel(heads, func(hd int) {
		scores := make([]float32, T)
		for i := 0; i < T; i++ {
			q := qkv[i*3*hidden+hd*headDim : i*3*hidden+hd*headDim+headDim]
			lo, hi := max(0, i-window), min(T-1, i+window)
			mx := float32(math.Inf(-1))
			for j := lo; j <= hi; j++ {
				k := qkv[j*3*hidden+hidden+hd*headDim : j*3*hidden+hidden+hd*headDim+headDim]
				s := dot(q, k) * scale
				scores[j] = s
				if s > mx {
					mx = s
				}
			}
			var sum float32
			for j := lo; j <= hi; j++ {
				e := float32(math.Exp(float64(scores[j] - mx)))
				scores[j] = e
				sum += e
			}
			out := dst[i*hidden+hd*headDim : i*hidden+hd*headDim+headDim]
			clear(out)
			for j := lo; j <= hi; j++ {
				p := scores[j] / sum
				v := qkv[j*3*hidden+2*hidden+hd*headDim : j*3*hidden+2*hidden+hd*headDim+headDim]
				for d := range out {
					out[d] += p * v[d]
				}
			}
		}
	})
}

// matmulT computes dst[T, out] = x[T, in] · Wᵀ for a PyTorch-layout weight W[out, in], split across
// the CPUs by output rows. Each worker walks 4 weight rows × 4 tokens at a time so every loaded value
// feeds four multiply-adds.
func matmulT(dst, x, w []float32, T, in, out int) {
	const blk = 64
	nblk := (out + blk - 1) / blk
	parallel(nblk, func(b int) {
		o0, o1 := b*blk, min(out, (b+1)*blk)
		o := o0
		for ; o+4 <= o1; o += 4 {
			w0, w1, w2, w3 := w[o*in:(o+1)*in], w[(o+1)*in:(o+2)*in], w[(o+2)*in:(o+3)*in], w[(o+3)*in:(o+4)*in]
			t := 0
			for ; t+4 <= T; t += 4 {
				var r [4][4]float32
				x0, x1, x2, x3 := x[t*in:(t+1)*in], x[(t+1)*in:(t+2)*in], x[(t+2)*in:(t+3)*in], x[(t+3)*in:(t+4)*in]
				dot4x4(&r, x0, x1, x2, x3, w0, w1, w2, w3)
				for ti := 0; ti < 4; ti++ {
					row := dst[(t+ti)*out+o:]
					row[0], row[1], row[2], row[3] = r[ti][0], r[ti][1], r[ti][2], r[ti][3]
				}
			}
			for ; t < T; t++ {
				xt := x[t*in : (t+1)*in]
				row := dst[t*out+o:]
				row[0], row[1], row[2], row[3] = dot(xt, w0), dot(xt, w1), dot(xt, w2), dot(xt, w3)
			}
		}
		for ; o < o1; o++ {
			wo := w[o*in : (o+1)*in]
			for t := 0; t < T; t++ {
				dst[t*out+o] = dot(x[t*in:(t+1)*in], wo)
			}
		}
	})
}

func dot4x4(r *[4][4]float32, x0, x1, x2, x3, w0, w1, w2, w3 []float32) {
	n := len(x0)
	x1, x2, x3 = x1[:n], x2[:n], x3[:n]
	w0, w1, w2, w3 = w0[:n], w1[:n], w2[:n], w3[:n]
	var a00, a01, a02, a03, a10, a11, a12, a13, a20, a21, a22, a23, a30, a31, a32, a33 float32
	for i := 0; i < n; i++ {
		b0, b1, b2, b3 := w0[i], w1[i], w2[i], w3[i]
		v := x0[i]
		a00 += v * b0
		a01 += v * b1
		a02 += v * b2
		a03 += v * b3
		v = x1[i]
		a10 += v * b0
		a11 += v * b1
		a12 += v * b2
		a13 += v * b3
		v = x2[i]
		a20 += v * b0
		a21 += v * b1
		a22 += v * b2
		a23 += v * b3
		v = x3[i]
		a30 += v * b0
		a31 += v * b1
		a32 += v * b2
		a33 += v * b3
	}
	*r = [4][4]float32{{a00, a01, a02, a03}, {a10, a11, a12, a13}, {a20, a21, a22, a23}, {a30, a31, a32, a33}}
}

func dot(a, b []float32) float32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

// parallel runs f(0..n-1) across GOMAXPROCS workers.
func parallel(n int, f func(i int)) {
	workers := min(n, runtime.GOMAXPROCS(0))
	if workers <= 1 {
		for i := 0; i < n; i++ {
			f(i)
		}
		return
	}
	var wg sync.WaitGroup
	next := make(chan int, n)
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	wg.Wait()
}
