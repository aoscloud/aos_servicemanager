// Package fcrypt Provides cryptographic interfaces for Fusion
package fcrypt

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"

	log "github.com/sirupsen/logrus"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/config"
)

var fcryptCfg config.Crypt

// Init initializes of fcrypt package
func Init(conf config.Crypt) {
	fcryptCfg = conf

	log.Debug("CAcert:         ", fcryptCfg.CACert)
	log.Debug("ClientCert:     ", fcryptCfg.ClientCert)
	log.Debug("ClientKey:      ", fcryptCfg.ClientKey)
	log.Debug("OfflinePrivKey: ", fcryptCfg.OfflinePrivKey)
	log.Debug("OfflineCert:    ", fcryptCfg.OfflineCert)
}

func verifyCert(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return nil
}

func getCaCertPool() (*x509.CertPool, error) {
	// Load CA cert
	caCert, err := ioutil.ReadFile(fcryptCfg.CACert)
	if err != nil {
		log.Errorf("Error reading CA certificate: %s", err)
		return nil, err
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	return caCertPool, nil
}

// GetTLSConfig Provides TLS configuration which can be used with HTTPS client
func GetTLSConfig() (*tls.Config, error) {
	// Load client cert
	cert, err := tls.LoadX509KeyPair(fcryptCfg.ClientCert, fcryptCfg.ClientKey)
	if err != nil {
		log.Fatalf("Can't load key pair: %s", err)
	}

	caCertPool, err := getCaCertPool()
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates:          []tls.Certificate{cert},
		RootCAs:               caCertPool,
		VerifyPeerCertificate: verifyCert,
	}

	tlsConfig.BuildNameToCertificate()
	return tlsConfig, nil
}

func getPrivKey() (*rsa.PrivateKey, error) {
	var err error
	var key crypto.PrivateKey

	keyBytes, err := ioutil.ReadFile(fcryptCfg.OfflinePrivKey)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error reading private key: %s", err))
	}

	if key, err = decodePrivateKey(keyBytes); err != nil {
		return nil, err
	}

	switch key := key.(type) {
	case *rsa.PrivateKey:
		return key, nil
	default: // can be only  *ecdsa.PrivateKey after decodePrivateKey
		return nil, errors.New("ECDSA private key not yet supported")
	}
}

func getOfflineCert() (*x509.Certificate, error) {
	pemCert, err := ioutil.ReadFile(fcryptCfg.OfflineCert)
	if err != nil {
		log.Errorf("Error reading offline certificate: %s", err)
		return nil, err
	}

	var block *pem.Block
	block, _ = pem.Decode(pemCert)
	if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
		return nil, errors.New("Invalid PEM Block")
	}

	return x509.ParseCertificate(block.Bytes)
}

func extractKeyFromCert(cert *x509.Certificate) (*rsa.PublicKey, error) {
	switch pub := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return pub, nil
	default:
		return nil, errors.New("Unknown public key type")
	}

}

func decrypt(fin *os.File, fout *os.File, key []byte, iv []byte,
	cryptoAlg string, cryptoMode string) (err error) {

	log.Debugf("IV: %v", iv)
	log.Debugf("Key: %v", key)

	var mode cipher.BlockMode
	var block cipher.Block

	switch cryptoAlg {
	case "AES128", "AES192", "AES256":
		block, err = aes.NewCipher(key)
		if err != nil {
			log.Errorf("Can't create cipher: %s", err)
			return err
		}
	default:
		return errors.New("Unknown cryptographic algorithm: " + cryptoAlg)
	}

	switch cryptoMode {
	case "CBC":
		mode = cipher.NewCBCDecrypter(block, iv)
	default:
		return errors.New("Unknown encryption mode: " + cryptoMode)
	}

	indata, err := ioutil.ReadAll(fin)
	if err != nil {
		log.Errorf("Can't read image file: %s", err)
		return err
	}

	outdata := make([]byte, len(indata))
	mode.CryptBlocks(outdata, indata)
	outdata, err = removePkcs7Padding(outdata, mode.BlockSize())
	if err != nil {
		return err
	}

	fout.Write(outdata)
	return nil
}

func parseCertificates(pemData string) (ret []*x509.Certificate, err error) {

	for block, remainder := pem.Decode([]byte(pemData)); block != nil; block, remainder = pem.Decode(remainder) {
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("Invalid PEM Block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			log.Errorf("Error parsing certificate: %s", err)
			return nil, err
		}
		ret = append(ret, cert)
	}
	return
}
func getAndVerifySignCert(certificates string) (ret *x509.Certificate, err error) {
	certs, err := parseCertificates(certificates)
	if err != nil {
		return nil, err
	}

	if len(certs) == 0 {
		return nil, errors.New("No certificates found in certificate chain")
	}

	signCertificate := certs[0]

	intermediatePool := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediatePool.AddCert(cert)
	}

	caCertPool, err := getCaCertPool()
	if err != nil {
		return
	}

	verifyOptions := x509.VerifyOptions{
		Intermediates: intermediatePool,
		Roots:         caCertPool,
		// TODO: Use more sensible ExtKeyUsage
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	_, err = signCertificate.Verify(verifyOptions)
	if err != nil {
		log.Error("Error verifying certificate chain")
		return
	}

	return signCertificate, nil
}

func checkSign(f *os.File, signatureAlg, hashAlg, signatureScheme string,
	signature []byte, certificates string) error {

	signCert, err := getAndVerifySignCert(certificates)
	if err != nil {
		return err
	}
	f.Seek(0, 0)

	var hashFunc crypto.Hash
	switch hashAlg {
	case "SHA1":
		hashFunc = crypto.SHA1
	case "SHA224":
		hashFunc = crypto.SHA224
	case "SHA256":
		hashFunc = crypto.SHA256
	case "SHA384":
		hashFunc = crypto.SHA384
	case "SHA512":
		hashFunc = crypto.SHA512
	case "SHA512/224":
		hashFunc = crypto.SHA512_224
	case "SHA512/256":
		hashFunc = crypto.SHA512_256
	default:
		return errors.New("Unknown hashing algorithm: " + hashAlg)
	}

	hash := hashFunc.New()
	_, err = io.Copy(hash, f)
	if err != nil {
		log.Errorf("Error hashing file: %s", err)
		return err
	}
	h := hash.Sum(nil)

	log.Debugf("Hash: %s", hex.EncodeToString(h))

	switch signatureAlg {
	case "RSA":
		key, err := extractKeyFromCert(signCert)
		if err != nil {
			return err
		}
		switch signatureScheme {
		case "PKCS1v1_5":
			return rsa.VerifyPKCS1v15(key, hashFunc.HashFunc(), h, signature)
		case "PSS":
			return rsa.VerifyPSS(key, hashFunc.HashFunc(), h, signature, nil)
		default:
			return errors.New("Unknown scheme for RSA signature: " + signatureScheme)
		}
	default:
		return errors.New("Unknown signature alg: " + signatureAlg)
	}
}

func removePkcs7Padding(in []byte, blocklen int) ([]byte, error) {
	l := len(in)
	if l%blocklen != 0 {
		return nil, errors.New("Padding error")
	}

	pl := int(in[l-1])
	if pl < 1 || pl > blocklen {
		return nil, errors.New("Padding error")
	}

	for i := l - pl; i < l; i++ {
		if in[i] != byte(pl) {
			return nil, errors.New("Padding error")
		}
	}

	return in[0 : l-pl], nil
}

//DecryptMetadata decrypt service metadata
func DecryptMetadata(der []byte) ([]byte, error) {
	cert, err := getOfflineCert()
	if err != nil {
		return nil, err
	}

	key, err := getPrivKey()
	if err != nil {
		return nil, err
	}

	return DecryptMessage(der, key, cert)
}

//DecryptImage Decrypts given image into temporary file
func DecryptImage(fname string, signature []byte, key []byte, iv []byte,
	signatureAlg, hashAlg, signatureScheme,
	cryptoAlg, cryptoMode, certificates string) (outfname string, err error) {

	// Create tmp file with output data
	fout, err := ioutil.TempFile("", "fcrypt")
	if err != nil {
		log.Errorf("Can't create temp file for image: %s", err)
		return outfname, err
	}
	defer fout.Close()

	log.Debugf("Temp file name: %s", fout.Name())

	// Open file
	f, err := os.Open(fname)
	if err != nil {
		log.Errorf("Error opening encrypted image:%s", err)
		return outfname, err
	}
	defer f.Close()

	// Decrypt file into fout
	err = decrypt(f, fout, key, iv, cryptoAlg, cryptoMode)
	if err != nil {
		log.Errorf("Error decrypting file: %s", err)
		return outfname, err
	}

	err = checkSign(fout, signatureAlg, hashAlg, signatureScheme, signature, certificates)
	if err != nil {
		log.Errorf("Signature verify error: %s", err)
		return outfname, err
	}
	return fout.Name(), nil
}
