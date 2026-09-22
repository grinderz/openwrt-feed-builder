package feedbuilder

import "errors"

// Sentinel errors, one per failure domain; call sites wrap them with the
// details (errors.Is works on any of them).
var (
	errADB             = errors.New("adb")
	errIPK             = errors.New("ipk")
	errAPKTool         = errors.New("apk-tools")
	errUsign           = errors.New("usign")
	errUPX             = errors.New("upx")
	errKey             = errors.New("key")
	errBadMode         = errors.New("bad mode")
	errArchive         = errors.New("archive")
	errConfig          = errors.New("config")
	errSource          = errors.New("source")
	errBinarySource    = errors.New("binary source")
	errSDKSource       = errors.New("sdk source")
	errHTTPStatus      = errors.New("unexpected HTTP status")
	errGitHub          = errors.New("GitHub API")
	errDownload        = errors.New("download")
	errOutputDir       = errors.New("output directory")
	errSignUnavailable = errors.New("signing unavailable")
	errIndex           = errors.New("index")
	errVerify          = errors.New("verify")
)
