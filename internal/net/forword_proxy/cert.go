package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// CertManager 证书管理器
type CertManager struct {
	caCert     *x509.Certificate
	caKey      *rsa.PrivateKey
	certCache  map[string]*tls.Certificate
	cacheMutex sync.RWMutex
}

// NewCertManager 创建新的证书管理器
func NewCertManager(certFile, keyFile string) (*CertManager, error) {
	cm := &CertManager{
		certCache: make(map[string]*tls.Certificate),
	}

	// 尝试加载现有的CA证书和私钥
	if err := cm.loadCA(certFile, keyFile); err != nil {
		// 如果加载失败，生成新的CA证书和私钥
		if err := cm.generateCA(certFile, keyFile); err != nil {
			return nil, fmt.Errorf("生成CA证书失败: %w", err)
		}
	}

	return cm, nil
}

// loadCA 加载CA证书和私钥
func (cm *CertManager) loadCA(certFile, keyFile string) error {
	// 读取证书文件
	certPEM, err := ioutil.ReadFile(certFile)
	if err != nil {
		return err
	}

	// 读取私钥文件
	keyPEM, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return err
	}

	// 解析证书
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return fmt.Errorf("无法解析证书PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return err
	}

	// 解析私钥
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("无法解析私钥PEM")
	}

	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}

	cm.caCert = cert
	cm.caKey = key

	return nil
}

// generateCA 生成新的CA证书和私钥
func (cm *CertManager) generateCA(certFile, keyFile string) error {
	// 生成私钥
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// 创建证书模板
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization:  []string{"Proxy CA"},
			Country:       []string{"CN"},
			Province:      []string{""},
			Locality:      []string{""},
			StreetAddress: []string{""},
			PostalCode:    []string{""},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// 生成证书
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	// 解析生成的证书
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return err
	}

	// 保存证书到文件
	certOut, err := os.Create(certFile)
	if err != nil {
		return err
	}
	defer certOut.Close()

	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	// 保存私钥到文件
	keyOut, err := os.Create(keyFile)
	if err != nil {
		return err
	}
	defer keyOut.Close()

	pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	cm.caCert = cert
	cm.caKey = key

	return nil
}

// GetCertificate 为指定主机名生成或获取证书
func (cm *CertManager) GetCertificate(hostname string) (*tls.Certificate, error) {
	cm.cacheMutex.RLock()
	if cert, exists := cm.certCache[hostname]; exists {
		cm.cacheMutex.RUnlock()
		return cert, nil
	}
	cm.cacheMutex.RUnlock()

	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()

	// 双重检查
	if cert, exists := cm.certCache[hostname]; exists {
		return cert, nil
	}

	// 生成新证书
	cert, err := cm.generateCertificate(hostname)
	if err != nil {
		return nil, err
	}

	cm.certCache[hostname] = cert
	return cert, nil
}

// generateCertificate 为指定主机名生成证书
func (cm *CertManager) generateCertificate(hostname string) (*tls.Certificate, error) {
	// 生成私钥
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	// 创建证书模板
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"Proxy Server"},
			CommonName:   hostname,
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}

	// 如果hostname是IP地址，添加到IPAddresses
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	}

	// 生成证书
	certDER, err := x509.CreateCertificate(rand.Reader, &template, cm.caCert, &key.PublicKey, cm.caKey)
	if err != nil {
		return nil, err
	}

	// 创建TLS证书
	cert := &tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}

	return cert, nil
}