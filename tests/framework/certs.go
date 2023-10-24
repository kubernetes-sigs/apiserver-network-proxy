/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"fmt"
	"os"
	"path/filepath"
)

var CertsDir string // Initialized with InitCertsDir

// The certificates & keys in this file are insecure and for testing use only.
// Use `make certs` to generate certificates & keys for other uses.

const testCA = `-----BEGIN CERTIFICATE-----
MIIDSDCCAjCgAwIBAgIUWvgHsWuJgLnna8Yl8I+rBui0XGIwDQYJKoZIhvcNAQEL
BQAwFTETMBEGA1UEAwwKMTI3LjAuMC4xQDAeFw0yMzEwMDYyMDAxMzdaFw0zMzEw
MDMyMDAxMzdaMBUxEzARBgNVBAMMCjEyNy4wLjAuMUAwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQCCJ3zvMr+obQNH6W268sOWO089pl98cQ3r5CxkJrNk
I59dOAtaEnCYSo+WNUZI+7hsEUqHb/bnJQu4ydTA+Y5LEwwtW3kcDterVakPd7oG
i6XqxoQat+5wrXXhASmZnZv6ilgqOyDCJhJsT4QljdG4bHLJFfW8wWdrHZpII87a
OP3yvUPX0grQkBNKiwvxvmTpa4R2EysaEiBSemGF0kIF5FI7lfkjGXjQBNeFW74s
KW+aToflU2Tus5U9CXpOzjwYJZVKvAEV4FtjCjCIwIPc2VLx3nv89o7ui4ax1y3I
UrtlaPe7XWfWEUo9hMgo33+1SrMwWcJMRiM3KRtqD7XPAgMBAAGjgY8wgYwwHQYD
VR0OBBYEFNXp9CUZJqP3z9oVX92PGy0prsriMFAGA1UdIwRJMEeAFNXp9CUZJqP3
z9oVX92PGy0prsrioRmkFzAVMRMwEQYDVQQDDAoxMjcuMC4wLjFAghRa+Aexa4mA
uedrxiXwj6sG6LRcYjAMBgNVHRMEBTADAQH/MAsGA1UdDwQEAwIBBjANBgkqhkiG
9w0BAQsFAAOCAQEAH2xugrzi0T7CK17QEdsA5iING2Fvjf7Oe7L4c/knvlkaSeJZ
kEYJEGbzD8mMJPPz7Q8/zJMlFxbKOVrNxipUaFb2VJDcZg4fKbRwkUjIB+nvCrfy
fLei2XRgjW5fG94G0Z+E0hD6KQxY6wEAfyZnpsBJdETo4bYAkd5h4h6dwgFx66NF
ERjoxlSBVsnqksGID1/Q3pNdRP1si9OyLTs+WFVCbGW2O4+czY6EeGw9BEMtS35M
ZFow5H5OAshlvxbUftmK0TEnFhs509sKjgJVZMpSIr7ImqLPmRytJJ8zCoNQevuc
zVji1R3oGpF5D2FBzbtMuT3ydGX9DPxgt1s8Kw==
-----END CERTIFICATE-----
`

const testServerCert = `-----BEGIN CERTIFICATE-----
MIIDczCCAlugAwIBAgIBATANBgkqhkiG9w0BAQsFADAVMRMwEQYDVQQDDAoxMjcu
MC4wLjFAMB4XDTIzMTAwNjIwMDEzN1oXDTMzMTAwMzIwMDEzN1owGTEXMBUGA1UE
AwwOcHJveHktZnJvbnRlbmQwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIB
AQC5MCsFMaac22FuamGZcUhd2WoMTasFt//o3ondhRKhabFgUIoiQqVGUz/Pe/vI
Et5ixDU3wntGRBcceBMkOO9kWdFUQ1Rzxk1amg926ROA0LND4w2tVCsFHbCfny7A
PmnV40m3ayzIknGvnGNl52RP2vRTr1zocMkd3brRBESeAt2C/Fr8MjcsGLBIjAab
mp1P6bozOgjEZOA4gMCVVIl5VvZ2O1xgoVU7Q5RQBu+xydBmMcdKMjQh4IkxEuSH
HEJ9YVVZEmi11yqv+t6cTf2rnj32iW5oLJRbY+mzRPnfpTDsAM52gUXVJSGujohf
VTIZ9OrW3/8YXx2O7CBJFlWXAgMBAAGjgckwgcYwCQYDVR0TBAIwADAdBgNVHQ4E
FgQUK+SBt6V3wln+gF7YNxLeaIydk7kwUAYDVR0jBEkwR4AU1en0JRkmo/fP2hVf
3Y8bLSmuyuKhGaQXMBUxEzARBgNVBAMMCjEyNy4wLjAuMUCCFFr4B7FriYC552vG
JfCPqwbotFxiMBMGA1UdJQQMMAoGCCsGAQUFBwMBMAsGA1UdDwQEAwIFoDAmBgNV
HREEHzAdggprdWJlcm5ldGVzgglsb2NhbGhvc3SHBH8AAAEwDQYJKoZIhvcNAQEL
BQADggEBAETRBhlXT42bBm77k+C+lqc6EI+swinal1PmeLiOjm1o/66l4wF/XD3V
z167HsQlJ4cP6wMDOHhP7VLVxywhbwO43mXt0Q3SZ2vyJBjosmOC+8g1XLBL5MQT
NJjBjFf2mObWB6DM5XCfRLbMKA+odqoWl4sRvhqg8LuxhScb4Ul/IyT4HpULBhNT
oSjj8gGCbUT3qsERlopx6KUC1doHw3b/faKMfY0zSPfn5HIPWBdhE1FumXGuJ/oi
bauj4tNadWNkXrUQ01/aRnOb0zTCCbmvCbfWsjkdw6Oku9yfEvgndClY56O5lXOK
QvPH9fTYK2ruBXC9zikrAXTs6QpBAgY=
-----END CERTIFICATE-----
`

const testServerKey = `-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQC5MCsFMaac22Fu
amGZcUhd2WoMTasFt//o3ondhRKhabFgUIoiQqVGUz/Pe/vIEt5ixDU3wntGRBcc
eBMkOO9kWdFUQ1Rzxk1amg926ROA0LND4w2tVCsFHbCfny7APmnV40m3ayzIknGv
nGNl52RP2vRTr1zocMkd3brRBESeAt2C/Fr8MjcsGLBIjAabmp1P6bozOgjEZOA4
gMCVVIl5VvZ2O1xgoVU7Q5RQBu+xydBmMcdKMjQh4IkxEuSHHEJ9YVVZEmi11yqv
+t6cTf2rnj32iW5oLJRbY+mzRPnfpTDsAM52gUXVJSGujohfVTIZ9OrW3/8YXx2O
7CBJFlWXAgMBAAECggEAMv80QaRoJPb28ECkaux6yLloDkZPK+59LyQlZBbSyBeC
jKrxNzkSKXkgb+NNNU4Y5qrwms/YQcPbd3ALmWSCbCid0C4QcidwQtx9GLpbsBQI
4c+DgzFT/X8tFe/woGkvnQKP2M5PUVaerwUKjFP52FHMCcWXeL0ibTKT0R5zRO2+
Bfo2BmQlzskooq2qhGgzLZ6mXtnDQVTIeRdoB4kgtqZzzPzWO6S8oeSh4vH7gmoO
NzF9mUlYvbvYuQ2tZp+mLqb5qub5kLCIa1pGESnHWy58hV4X1uRSwGvH4WyR3A93
4OpD/Jv1KISgXFJW0QTBH4Ll2cvcqq1XmTNvWhV0wQKBgQDj3WoDyPkJiW+KfOIz
yQ+tPS+F8dyEe/TQltRHUUbJ69VpPhb5hAUjeOzX5FDjJl4P/4v1K5SCpNelFGeP
eXvAGX4kGh0zCX7SrEdQfsxR2oRWzew9zOJ4wdrz3H3cCBl5P2WZvaj/kaUoHmOW
HE/YLM/NWhNwgFdnBCJhJgBdVwKBgQDQDciyW3EeyOy3zxsizWXNAPo/o3yYFzdK
CghuleP/trXk6uk9VD69tiZ7hGMunjGMrgBbr9VjtkpXHdkX0C9TFjPFiRQVVF6g
SZ9g368uyulozXfRLbsfguN2SyS3zUV2CSj0uNh58kCiZ8uF9oOkv1IarRhh4DHz
klOqIt5hwQKBgQDKEsX8e1LW8Um4j81uTUUYxeUKLRX5a5ANF2VDpcFYOkt03Ho1
Zq3D6m5nevN8rb7HA0IT90TpotQWcoTwiLSFBFaIH5x7cVVF8UABE6GQiW/JJy71
E2hX3NqWXphC8+/bRayNbdOcaYYEkQaRzaPFOuBB5TrODxLzqYfvjWrPWwKBgQCP
gtKLZNP0njfa2jsnmHK+JAx6VTUeW/VBVwZV8YKh4tA5JWjZawEUL08AKGOZxnj7
RxLsK6+P5jAFQ4t6B5p9P3VarqFxzQ6wldggJGtcZY73QbOCUH8gz1JDSLX9KtTd
BJiBpfd8toOrAtm6gD5yJ55k1D1bViBemPKpCwBGgQKBgCN+2x414z/Ok4Bc79+M
nnL5xgORgGCcqWhfl9nEfcG+NqVu2WnyRoGmk5OPRavfkTnmqqnZhs508+WVKL6O
gBiLDxqRqqdCGSDEN06iUHseTiFmT7nSlOKVhOdUTVoxEXJYe7qD3Rj0U+QC1CNb
ZSwx3ZMKdQL8ikQf7cEZuV9O
-----END PRIVATE KEY-----
`

const testAgentCert = `-----BEGIN CERTIFICATE-----
MIIDSDCCAjCgAwIBAgIBAjANBgkqhkiG9w0BAQsFADAVMRMwEQYDVQQDDAoxMjcu
MC4wLjFAMB4XDTIzMTAwNjIwMDEzOFoXDTMzMTAwMzIwMDEzOFowFjEUMBIGA1UE
AwwLcHJveHktYWdlbnQwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDB
5emcdqRr2VJ23plXWRhDbfDe/lyqZ44lkfrjGhh6fIRIWfjnCYhhPSLiWiG0LIf1
RGSyLJ5jQcs8pwAMwE3KUc0tA4/whF088QXExOtnSlIvZG2pTXmeuMXxidlcv3jE
mI2Y0gcvvFhfkRuxckJSuvfaOjVbL1dBJe0W9m3oVqZLLYuZ8KGMvJBGWipjGlaE
EX3oP8S2ef7JwVMJ4vJRC+yGOIGoEDkUGFxiZLGxnq8qGCYEU8upOCCJt7LCvwQh
SB3/PQreq1q8qcc/sKsTF8QV09k+VbQ77n8HFGttDnbHYRPX6C9fNyU2bqwJOvd0
giUKAwyL7D/MnF6K9Z11AgMBAAGjgaEwgZ4wCQYDVR0TBAIwADAdBgNVHQ4EFgQU
9++eLNBpGZ6qurgprQQTaduj1PEwUAYDVR0jBEkwR4AU1en0JRkmo/fP2hVf3Y8b
LSmuyuKhGaQXMBUxEzARBgNVBAMMCjEyNy4wLjAuMUCCFFr4B7FriYC552vGJfCP
qwbotFxiMBMGA1UdJQQMMAoGCCsGAQUFBwMCMAsGA1UdDwQEAwIHgDANBgkqhkiG
9w0BAQsFAAOCAQEAPra+ZeyI5X86PZuHOSq/s8xWMEAo34B//N7ipv4yyYUtlcOl
WNBtRWi9gtnQz1NAZplsjxMDSKCTuZScNtUUMJLpoTPzfE2UdvLN4eZ2hJGKZLXD
qlljvKGTcFyzcxSXcO3lqWJP6jhnb5JIgiK3qqW/UXTY8DEN1h9P+v9lcP7oOjTP
smXGG+fREUlt0dyTkJWcP4m/84XmhRCbktQ7nYnk4f3Yq0eq8bkZ+BCAoMePrYf9
nTXUWUjxbwRWvbtd8bKm2BkWLeVyNYxxghUZg0wycIV556lkNARO+mN6Q3hDGx7n
zAAE7B/05pCD7R7zER4/I0S/rgZbyNFpq/N6tA==
-----END CERTIFICATE-----
`

const testAgentKey = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDB5emcdqRr2VJ2
3plXWRhDbfDe/lyqZ44lkfrjGhh6fIRIWfjnCYhhPSLiWiG0LIf1RGSyLJ5jQcs8
pwAMwE3KUc0tA4/whF088QXExOtnSlIvZG2pTXmeuMXxidlcv3jEmI2Y0gcvvFhf
kRuxckJSuvfaOjVbL1dBJe0W9m3oVqZLLYuZ8KGMvJBGWipjGlaEEX3oP8S2ef7J
wVMJ4vJRC+yGOIGoEDkUGFxiZLGxnq8qGCYEU8upOCCJt7LCvwQhSB3/PQreq1q8
qcc/sKsTF8QV09k+VbQ77n8HFGttDnbHYRPX6C9fNyU2bqwJOvd0giUKAwyL7D/M
nF6K9Z11AgMBAAECggEAManDxjGVN5J4Tr4BJKBLWKoGMfeQoIzZmcHkMtryPh06
fJWe7P5CEjXog3V2gIGPaUDVUdWf0+h8N9LGbn2q7xE4rjjlW0Nr5joNsjKF4PTm
TAE7HUwcxIyrFoyqQdlBA4nXarcQ5Ccns4KlRzPuzOXaqeiS1gIwJR2jtmf0CrgE
ykFyLGhfSvc/fiCr7EDxr/eOANx7mf6ONsPHOIEA/mvs/V48OndsZdgryrVvsvXJ
/Mn2iw/S6CbmQtTjW+2zbknh3wbRiVC3ZToCtg2xIYmh04TjW+eb3Ly84BAflMbB
+nTuT8cl7qe407RaAX5YOxLyog86yPKUDXqRl1CZmQKBgQDfWMJ4uIopTNBpjgIK
yEq4HxYLLYcHanjjAhG15gwATmqr6h4q7uotR1Lstz2n//xrysDFTc90wB3eiciI
xcdmXrx+oDjVoiq1/lui2lqFgQkOPvkwaCa5u82qjrTW567jB7HrwhJ4KDf4Z3Z2
vjgzHGDldrzZKKujm9Rbylnt3wKBgQDePvlEBVKl84H32UDCKgiJHvZLOTRTLK1B
pJQpoEhjdFs6y3QFB3toK4AkYI6tFb94IaMnalF+aRccHu/VB1olbmbla2O6+92h
0OuXqRMOP6Fm+Z11u/+VI2y5T8KvcLAmoEDxSyjtsZaIK2bnJnbr2kry0WCpytCn
ag3IOLh3KwKBgQC1OzXadYwOxTjcXhIEI9CVpQvjGBdYiin7soMigcA9Q2RFiZzf
I6y7/wMn9+y89Pgjk4tmzqPHTdku6cjiSvJpe/giG+riV0unD/XVqK8JY9IwUCMu
B2VdEypo+pF9TNRZfrX94yXPgHsiQvoaknHR73Yk3HuTDvBvuxPPQ9xDKwKBgE7P
dgUw/gXrPANv/w7baPt3B0/VkUCNb0L/4aqBNCpQcKmAzDucU561DlPYCcBHHgaz
pu+rPArfqVpHfjTEzqrHY6WnV05PUmC3fVPimOdMmSezDKtbZ16zmTJ9nkQoac7I
tT7bsD/Z4c+X1H3Tngg0+K7yoJyVVziG2yxNMNzRAoGAFpFz1nci58g7UobjvM9q
TZoVJGWe0YlufRJ7TiWnHGqptgHr+Eot4a8PfKLikhDMXQUSf5VjqK23IrX+pW2R
DHNQLqjuhU8espwRnA1qzWd3Ss1LccVe/v+2EQcVio95mK4iEGlNXT1bkMcDGhFz
UMKSzlENDkz1zDPyaQmd9wI=
-----END PRIVATE KEY-----
`

const (
	TestCAFile         = "anp-test-ca.crt"
	TestServerCertFile = "anp-test-server.crt"
	TestServerKeyFile  = "anp-test-server.key"
	TestAgentCertFile  = "anp-test-agent.crt"
	TestAgentKeyFile   = "anp-test-agent.key"
)

// InitCertsDir writes the above certificates & keys to a temporary directory.
func InitCertsDir() error {
	dir, err := os.MkdirTemp("", "anp-testing")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	CertsDir = dir
	defer func() {
		if r := recover(); r != nil && err == nil {
			err = fmt.Errorf("panic: %v", r)
		} else if err == nil {
			return
		}
		// Something failed; clean up the temp directory.
		os.RemoveAll(dir)
	}()

	for filename, contents := range map[string]string{
		TestCAFile:         testCA,
		TestServerCertFile: testServerCert,
		TestServerKeyFile:  testServerKey,
		TestAgentCertFile:  testAgentCert,
		TestAgentKeyFile:   testAgentKey,
	} {
		if err := os.WriteFile(filepath.Join(dir, filename), []byte(contents), 0600); err != nil {
			return fmt.Errorf("failed to write test %s: %w", filename, err)
		}
	}

	return nil
}
