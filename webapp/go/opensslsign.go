package isuports

// TLS ハンドシェイクの RSA 署名を OpenSSL (cgo) で行う crypto.Signer。
// ベンチは参加者ごとに新規 TLS 接続を張り、セッション再開もしないので、ハンドシェイクごとに RSA-2048 の署名が 1 回走る。
// Go 標準 (crypto/internal/fips140/bigmod) より OpenSSL の方が速い（このマシンで openssl speed rsa2048 = 0.574ms/sign）。
// 起動時に自己テストして、だめなら Go 標準の鍵にフォールバックする。

/*
#cgo LDFLAGS: -lcrypto
#include <openssl/evp.h>
#include <openssl/pem.h>
#include <openssl/rsa.h>
#include <openssl/bio.h>

static EVP_PKEY* isu_load_key(const char* pem, int len) {
	BIO* b = BIO_new_mem_buf(pem, len);
	if (!b) return NULL;
	EVP_PKEY* k = PEM_read_bio_PrivateKey(b, NULL, NULL, NULL);
	BIO_free(b);
	return k;
}

static int isu_rsa_sign(EVP_PKEY* key, int pss, int mdnid, int saltlen,
		const unsigned char* digest, size_t dlen, unsigned char* sig, size_t* siglen) {
	EVP_PKEY_CTX* ctx = EVP_PKEY_CTX_new(key, NULL);
	if (!ctx) return 0;
	int ok = EVP_PKEY_sign_init(ctx) > 0
		&& EVP_PKEY_CTX_set_rsa_padding(ctx, pss ? RSA_PKCS1_PSS_PADDING : RSA_PKCS1_PADDING) > 0
		&& EVP_PKEY_CTX_set_signature_md(ctx, EVP_get_digestbynid(mdnid)) > 0
		&& (!pss || EVP_PKEY_CTX_set_rsa_pss_saltlen(ctx, saltlen) > 0)
		&& EVP_PKEY_sign(ctx, sig, siglen, digest, dlen) > 0;
	EVP_PKEY_CTX_free(ctx);
	return ok;
}
*/
import "C"

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// ISUCON_SIGN_PARALLEL=N で署名の同時実行数を制限する（未設定なら無制限）。
// 1 にするとハンドシェイクが詰まって ranking の 1.2 秒タイムアウトと再接続が増え、dial timeout が 235 件出た
var signSem = func() chan struct{} {
	n, _ := strconv.Atoi(os.Getenv("ISUCON_SIGN_PARALLEL"))
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}()

type opensslSigner struct {
	key *C.EVP_PKEY
	pub *rsa.PublicKey
}

func (s *opensslSigner) Public() crypto.PublicKey { return s.pub }

func (s *opensslSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	var nid C.int
	switch opts.HashFunc() {
	case crypto.SHA256:
		nid = C.NID_sha256
	case crypto.SHA384:
		nid = C.NID_sha384
	case crypto.SHA512:
		nid = C.NID_sha512
	default:
		return nil, errors.New("opensslSigner: unsupported hash")
	}
	pss, saltlen := C.int(0), C.int(0)
	if po, ok := opts.(*rsa.PSSOptions); ok {
		pss = 1
		switch po.SaltLength {
		case rsa.PSSSaltLengthEqualsHash:
			saltlen = C.int(opts.HashFunc().Size())
		case rsa.PSSSaltLengthAuto:
			saltlen = C.int(s.pub.Size() - 2 - opts.HashFunc().Size())
		default:
			saltlen = C.int(po.SaltLength)
		}
	}
	if len(digest) == 0 {
		return nil, errors.New("opensslSigner: empty digest")
	}
	sig := make([]byte, s.pub.Size())
	siglen := C.size_t(len(sig))
	// 同時に走らせる署名は 1 本だけ。2 vCPU は同じ物理コアの HT で、2 並列にしても署名のスループットは増えない
	// （benchsign: 1 並列 1677/s、2 並列 1766/s）。接続のバーストで両方の vCPU を署名が占有すると既存接続の処理が待たされる
	if signSem != nil {
		signSem <- struct{}{}
		defer func() { <-signSem }()
	}
	if C.isu_rsa_sign(s.key, pss, nid, saltlen,
		(*C.uchar)(unsafe.Pointer(&digest[0])), C.size_t(len(digest)),
		(*C.uchar)(unsafe.Pointer(&sig[0])), &siglen) == 0 {
		return nil, errors.New("opensslSigner: sign failed")
	}
	return sig[:siglen], nil
}

// 証明書を読み、RSA 鍵なら署名を OpenSSL に差し替える。自己テストに失敗したら Go 標準のまま返す
func loadTLSCertificate(certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return cert, err
	}
	goKey, ok := cert.PrivateKey.(*rsa.PrivateKey)
	if !ok || getEnv("ISUCON_OPENSSL_SIGN", "1") != "1" {
		return cert, nil
	}
	pem, err := os.ReadFile(keyFile)
	if err != nil || len(pem) == 0 {
		return cert, nil
	}
	k := C.isu_load_key((*C.char)(unsafe.Pointer(&pem[0])), C.int(len(pem)))
	if k == nil {
		return cert, nil
	}
	s := &opensslSigner{key: k, pub: &goKey.PublicKey}
	// 自己テスト: PSS と PKCS#1 v1.5 の両方を Go 側で検証する
	d := sha256.Sum256([]byte("isuports self test"))
	pssOpts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
	sig, err := s.Sign(rand.Reader, d[:], pssOpts)
	if err != nil || rsa.VerifyPSS(s.pub, crypto.SHA256, d[:], sig, pssOpts) != nil {
		return cert, nil
	}
	sig, err = s.Sign(rand.Reader, d[:], crypto.SHA256)
	if err != nil || rsa.VerifyPKCS1v15(s.pub, crypto.SHA256, d[:], sig) != nil {
		return cert, nil
	}
	cert.PrivateKey = s
	return cert, nil
}

// BenchSign は署名 1 回あたりの時間を測る（`isuports benchsign`。計測用）
func BenchSign() {
	certFile, keyFile := getEnv("ISUCON_TLS_CERT", "/etc/nginx/tls/fullchain.pem"), getEnv("ISUCON_TLS_KEY", "/etc/nginx/tls/key.pem")
	goCert, _ := tls.LoadX509KeyPair(certFile, keyFile)
	osCert, _ := loadTLSCertificate(certFile, keyFile)
	d := sha256.Sum256([]byte("bench"))
	opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
	run := func(name string, s crypto.Signer, par, n int) {
		start := time.Now()
		var wg sync.WaitGroup
		for p := 0; p < par; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < n; i++ {
					if _, err := s.Sign(rand.Reader, d[:], opts); err != nil {
						panic(err)
					}
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		fmt.Printf("%-8s par=%d  %.3f ms/sign (wall per goroutine)  %.0f sign/s total\n", name, par, float64(el.Microseconds())/1000/float64(n), float64(par*n)/el.Seconds())
	}
	for _, par := range []int{1, 2} {
		run("go", goCert.PrivateKey.(crypto.Signer), par, 1000)
		run("openssl", osCert.PrivateKey.(crypto.Signer), par, 1000)
	}
}
