package test

import "fetchit/modules"

type (
	FileMeta      = modules.FileMeta
	RenameOptions = modules.RenameOptions
	Config        = modules.Config
	TidalURLInfo  = modules.TidalURLInfo
	QobuzURLInfo  = modules.QobuzURLInfo
)

var (
	generateFilename         = modules.GenerateFilename
	sanitizeBaseName         = modules.SanitizeBaseName
	resolvePath              = modules.ResolvePath
	moveFile                 = modules.MoveFile
	ErrNoNamerInput          = modules.ErrNoNamerInput
	ErrNoNamingFields        = modules.ErrNoNamingFields
	isQobuzURL               = modules.IsQobuzURL
	parseQobuzURL            = modules.ParseQobuzURL
	computeQobuzRequestSig   = modules.ComputeQobuzRequestSig
	getActiveQobuzSession    = modules.GetActiveQobuzSession
	buildYtdlpOutputTemplate = modules.BuildYtdlpOutputTemplate
	cleanupIntermediateFiles = modules.CleanupIntermediateFiles
	isTidalURL               = modules.IsTidalURL
	parseTidalURL            = modules.ParseTidalURL
	parseDashManifest        = modules.ParseDashManifest
	mapTidalQuality          = modules.MapTidalQuality
	getTidalCoverURL         = modules.GetTidalCoverURL
)
