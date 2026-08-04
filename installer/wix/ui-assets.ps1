Set-StrictMode -Version Latest

function ConvertTo-GBaseLiteRtfText {
    param([Parameter(Mandatory)][string]$Value)

    $builder = [System.Text.StringBuilder]::new()
    foreach ($character in $Value.ToCharArray()) {
        $codePoint = [int]$character
        if ($codePoint -eq 13) {
            continue
        }
        if ($codePoint -eq 10) {
            [void]$builder.Append("\par`r`n")
            continue
        }
        if ($character -eq '\') {
            [void]$builder.Append('\\')
            continue
        }
        if ($character -eq '{') {
            [void]$builder.Append('\{')
            continue
        }
        if ($character -eq '}') {
            [void]$builder.Append('\}')
            continue
        }
        if ($codePoint -ge 32 -and $codePoint -le 126) {
            [void]$builder.Append($character)
            continue
        }
        if ($codePoint -gt 32767) {
            $codePoint -= 65536
        }
        [void]$builder.Append("\u${codePoint}?")
    }
    return $builder.ToString()
}

function New-GBaseLiteLicenseRtf {
    param(
        [Parameter(Mandatory)][string]$SourcePath,
        [Parameter(Mandatory)][string]$DestinationPath
    )

    $strictUtf8 = [System.Text.UTF8Encoding]::new($false, $true)
    $licenseText = [System.IO.File]::ReadAllText($SourcePath, $strictUtf8)
    if ($licenseText.Length -lt 500 -or -not $licenseText.Contains('GBaseLite') -or
        -not $licenseText.Contains('LICENSE')) {
        throw "The Chinese MSI license is incomplete"
    }

    $rtf = [System.Text.StringBuilder]::new()
    [void]$rtf.Append('{\rtf1\ansi\ansicpg936\uc1\deff0')
    [void]$rtf.Append('{\fonttbl{\f0\fnil\fcharset134 Microsoft YaHei UI;}}')
    [void]$rtf.Append('{\colortbl;\red14\green74\blue69;\red63\green70\blue78;}')
    [void]$rtf.Append("\viewkind4\pard\f0\cf2\fs20`r`n")

    $lines = $licenseText -split '\r?\n'
    for ($index = 0; $index -lt $lines.Count; $index++) {
        $line = $lines[$index].Trim()
        if ($line.Length -eq 0) {
            [void]$rtf.Append("\par`r`n")
            continue
        }
        $escaped = ConvertTo-GBaseLiteRtfText -Value $line
        if ($index -eq 0) {
            [void]$rtf.Append("\pard\sa240\cf1\fs32\b $escaped\b0\par`r`n")
        } elseif ($line.IndexOf([char]0x3001) -ge 1 -and $line.IndexOf([char]0x3001) -le 3) {
            [void]$rtf.Append("\pard\sb160\sa80\cf1\fs22\b $escaped\b0\par`r`n")
        } else {
            [void]$rtf.Append("\pard\sa100\cf2\fs20 $escaped\par`r`n")
        }
    }
    [void]$rtf.Append('}')
    [System.IO.File]::WriteAllText($DestinationPath, $rtf.ToString(), [System.Text.Encoding]::ASCII)
}

function New-GBaseLiteRoundedRectangle {
    param(
        [Parameter(Mandatory)][float]$X,
        [Parameter(Mandatory)][float]$Y,
        [Parameter(Mandatory)][float]$Width,
        [Parameter(Mandatory)][float]$Height,
        [Parameter(Mandatory)][float]$Radius
    )

    $diameter = $Radius * 2
    $path = [System.Drawing.Drawing2D.GraphicsPath]::new()
    $path.AddArc($X, $Y, $diameter, $diameter, 180, 90)
    $path.AddArc($X + $Width - $diameter, $Y, $diameter, $diameter, 270, 90)
    $path.AddArc($X + $Width - $diameter, $Y + $Height - $diameter, $diameter, $diameter, 0, 90)
    $path.AddArc($X, $Y + $Height - $diameter, $diameter, $diameter, 90, 90)
    $path.CloseFigure()
    return $path
}

function Draw-GBaseLiteDatabaseMark {
    param(
        [Parameter(Mandatory)][System.Drawing.Graphics]$Graphics,
        [Parameter(Mandatory)][float]$X,
        [Parameter(Mandatory)][float]$Y,
        [Parameter(Mandatory)][float]$Width,
        [Parameter(Mandatory)][float]$Height,
        [Parameter(Mandatory)][System.Drawing.Color]$MainColor,
        [Parameter(Mandatory)][System.Drawing.Color]$AccentColor
    )

    $strokeWidth = [Math]::Max(1.0, $Width / 18.0)
    $mainPen = [System.Drawing.Pen]::new($MainColor, $strokeWidth)
    $mainPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $mainPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $accentPen = [System.Drawing.Pen]::new($AccentColor, $strokeWidth)
    $accentPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $accentPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    try {
        $ellipseHeight = $Height * 0.28
        $Graphics.DrawEllipse($mainPen, $X, $Y, $Width, $ellipseHeight)
        $Graphics.DrawLine($mainPen, $X, $Y + ($ellipseHeight / 2), $X, $Y + $Height - ($ellipseHeight / 2))
        $Graphics.DrawLine($mainPen, $X + $Width, $Y + ($ellipseHeight / 2), $X + $Width, $Y + $Height - ($ellipseHeight / 2))
        $Graphics.DrawArc($mainPen, $X, $Y + $Height - $ellipseHeight, $Width, $ellipseHeight, 0, 180)
        $Graphics.DrawArc($accentPen, $X, $Y + ($Height * 0.32), $Width, $ellipseHeight, 0, 180)
        $Graphics.DrawArc($mainPen, $X, $Y + ($Height * 0.57), $Width, $ellipseHeight, 0, 180)
    } finally {
        $accentPen.Dispose()
        $mainPen.Dispose()
    }
}

function New-GBaseLiteApplicationIcon {
    param([Parameter(Mandatory)][string]$DestinationPath)

    $sizes = @(16, 20, 24, 32, 40, 48, 64, 128, 256)
    $images = [System.Collections.ArrayList]::new()
    foreach ($size in $sizes) {
        $bitmap = [System.Drawing.Bitmap]::new($size, $size, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
        $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
        $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
        $graphics.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
        $graphics.Clear([System.Drawing.Color]::Transparent)
        $background = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::FromArgb(14, 74, 69))
        $path = New-GBaseLiteRoundedRectangle -X 0.5 -Y 0.5 -Width ($size - 1.0) -Height ($size - 1.0) -Radius ([Math]::Max(2.0, $size * 0.18))
        try {
            $graphics.FillPath($background, $path)
            $padding = $size * 0.23
            Draw-GBaseLiteDatabaseMark -Graphics $graphics -X $padding -Y ($size * 0.20) -Width ($size - (2 * $padding)) -Height ($size * 0.58) -MainColor ([System.Drawing.Color]::White) -AccentColor ([System.Drawing.Color]::FromArgb(217, 164, 65))
            $stream = [System.IO.MemoryStream]::new()
            try {
                $bitmap.Save($stream, [System.Drawing.Imaging.ImageFormat]::Png)
                [void]$images.Add($stream.ToArray())
            } finally {
                $stream.Dispose()
            }
        } finally {
            $path.Dispose()
            $background.Dispose()
            $graphics.Dispose()
            $bitmap.Dispose()
        }
    }

    $fileStream = [System.IO.File]::Open($DestinationPath, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write)
    $writer = [System.IO.BinaryWriter]::new($fileStream)
    try {
        $writer.Write([uint16]0)
        $writer.Write([uint16]1)
        $writer.Write([uint16]$sizes.Count)
        $imageOffset = 6 + (16 * $sizes.Count)
        for ($index = 0; $index -lt $sizes.Count; $index++) {
            if ($sizes[$index] -eq 256) {
                $sizeByte = 0
            } else {
                $sizeByte = $sizes[$index]
            }
            $writer.Write([byte]$sizeByte)
            $writer.Write([byte]$sizeByte)
            $writer.Write([byte]0)
            $writer.Write([byte]0)
            $writer.Write([uint16]1)
            $writer.Write([uint16]32)
            $writer.Write([uint32]$images[$index].Length)
            $writer.Write([uint32]$imageOffset)
            $imageOffset += $images[$index].Length
        }
        foreach ($image in $images) {
            $writer.Write($image)
        }
    } finally {
        $writer.Dispose()
        $fileStream.Dispose()
    }
}

function New-GBaseLiteBannerBitmap {
    param([Parameter(Mandatory)][string]$DestinationPath)

    $bitmap = [System.Drawing.Bitmap]::new(493, 58, [System.Drawing.Imaging.PixelFormat]::Format24bppRgb)
    $bitmap.SetResolution(96, 96)
    $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
    $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $white = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::White)
    $brand = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::FromArgb(14, 74, 69))
    $gold = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::FromArgb(217, 164, 65))
    $font = [System.Drawing.Font]::new('Segoe UI Semibold', 15, [System.Drawing.FontStyle]::Bold, [System.Drawing.GraphicsUnit]::Pixel)
    try {
        $graphics.FillRectangle($white, 0, 0, 493, 58)
        $graphics.FillRectangle($brand, 348, 0, 145, 58)
        $graphics.FillRectangle($gold, 348, 0, 5, 58)
        Draw-GBaseLiteDatabaseMark -Graphics $graphics -X 365 -Y 13 -Width 26 -Height 31 -MainColor ([System.Drawing.Color]::White) -AccentColor ([System.Drawing.Color]::FromArgb(217, 164, 65))
        $graphics.DrawString('GBaseLite', $font, $white, 400, 19)
        $bitmap.Save($DestinationPath, [System.Drawing.Imaging.ImageFormat]::Bmp)
    } finally {
        $font.Dispose()
        $gold.Dispose()
        $brand.Dispose()
        $white.Dispose()
        $graphics.Dispose()
        $bitmap.Dispose()
    }
}

function New-GBaseLiteDialogBitmap {
    param([Parameter(Mandatory)][string]$DestinationPath)

    $bitmap = [System.Drawing.Bitmap]::new(493, 312, [System.Drawing.Imaging.PixelFormat]::Format24bppRgb)
    $bitmap.SetResolution(96, 96)
    $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
    $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $white = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::White)
    $brand = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::FromArgb(14, 74, 69))
    $gold = [System.Drawing.SolidBrush]::new([System.Drawing.Color]::FromArgb(217, 164, 65))
    $titleFont = [System.Drawing.Font]::new('Segoe UI Semibold', 21, [System.Drawing.FontStyle]::Bold, [System.Drawing.GraphicsUnit]::Pixel)
    $captionFont = [System.Drawing.Font]::new('Segoe UI', 9, [System.Drawing.FontStyle]::Regular, [System.Drawing.GraphicsUnit]::Pixel)
    try {
        $graphics.FillRectangle($white, 0, 0, 493, 312)
        $graphics.FillRectangle($brand, 0, 0, 150, 312)
        $graphics.FillRectangle($gold, 150, 0, 4, 312)
        Draw-GBaseLiteDatabaseMark -Graphics $graphics -X 43 -Y 62 -Width 64 -Height 76 -MainColor ([System.Drawing.Color]::White) -AccentColor ([System.Drawing.Color]::FromArgb(217, 164, 65))
        $graphics.DrawString('GBaseLite', $titleFont, $white, 26, 164)
        $graphics.DrawString('DATABASE SERVICE', $captionFont, $white, 34, 196)
        $bitmap.Save($DestinationPath, [System.Drawing.Imaging.ImageFormat]::Bmp)
    } finally {
        $captionFont.Dispose()
        $titleFont.Dispose()
        $gold.Dispose()
        $brand.Dispose()
        $white.Dispose()
        $graphics.Dispose()
        $bitmap.Dispose()
    }
}

function New-GBaseLiteMsiUiAssets {
    param(
        [Parameter(Mandatory)][string]$LicenseSourcePath,
        [Parameter(Mandatory)][string]$OutputDirectory
    )

    Add-Type -AssemblyName System.Drawing
    New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
    $licenseRtf = Join-Path $OutputDirectory 'license.zh-CN.rtf'
    $bannerBitmap = Join-Path $OutputDirectory 'banner.bmp'
    $dialogBitmap = Join-Path $OutputDirectory 'dialog.bmp'
    $applicationIcon = Join-Path $OutputDirectory 'gbaselite.ico'
    New-GBaseLiteLicenseRtf -SourcePath $LicenseSourcePath -DestinationPath $licenseRtf
    New-GBaseLiteBannerBitmap -DestinationPath $bannerBitmap
    New-GBaseLiteDialogBitmap -DestinationPath $dialogBitmap
    New-GBaseLiteApplicationIcon -DestinationPath $applicationIcon
    return [pscustomobject]@{
        LicenseRtf = $licenseRtf
        BannerBitmap = $bannerBitmap
        DialogBitmap = $dialogBitmap
        ApplicationIcon = $applicationIcon
    }
}
