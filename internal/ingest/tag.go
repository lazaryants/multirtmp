package ingest

import (
	"bytes"
	"fmt"

	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

const (
	kindAudio uint8 = 8
	kindVideo uint8 = 9
	kindData  uint8 = 18

	videoFrameTypeKey = 1

	videoCodecIDAVC = 7

	avcPacketTypeSeqHeader = 0

	soundFormatAAC = 10

	aacPacketTypeSeqHeader = 0
)

// videoTagInfo — разобранный первый байт(ы) FLV video-тега.
type videoTagInfo struct {
	frameType     uint8
	codecID       uint8
	isKeyFrame    bool
	isAVC         bool
	avcPacketType uint8
	isSeqHeader   bool
}

func parseVideoTag(data []byte) (videoTagInfo, error) {
	if len(data) < 1 {
		return videoTagInfo{}, fmt.Errorf("empty video tag")
	}

	b := data[0]
	frameType := b >> 4
	codecID := b & 0x0f

	info := videoTagInfo{
		frameType: frameType,
		codecID:   codecID,
		isAVC:     codecID == videoCodecIDAVC,
	}

	if info.isAVC {
		if len(data) < 2 {
			return videoTagInfo{}, fmt.Errorf("AVC video tag shorter than header (%d bytes)", len(data))
		}
		info.avcPacketType = data[1]
		info.isSeqHeader = info.avcPacketType == avcPacketTypeSeqHeader
	}

	// AVC sequence header (SPS/PPS) несёт FrameType=1 по конвенции FLV, но
	// это не кадр с картинкой — его нельзя засчитывать как keyframe для
	// замера интервала между keyframe.
	info.isKeyFrame = frameType == videoFrameTypeKey && !info.isSeqHeader

	return info, nil
}

// audioTagInfo — разобранный первый байт(ы) FLV audio-тега.
type audioTagInfo struct {
	soundFormat   uint8
	isAAC         bool
	aacPacketType uint8
	isSeqHeader   bool
}

func parseAudioTag(data []byte) (audioTagInfo, error) {
	if len(data) < 1 {
		return audioTagInfo{}, fmt.Errorf("empty audio tag")
	}

	b := data[0]
	soundFormat := b >> 4

	info := audioTagInfo{
		soundFormat: soundFormat,
		isAAC:       soundFormat == soundFormatAAC,
	}

	if info.isAAC {
		if len(data) < 2 {
			return audioTagInfo{}, fmt.Errorf("AAC audio tag shorter than header (%d bytes)", len(data))
		}
		info.aacPacketType = data[1]
		info.isSeqHeader = info.aacPacketType == aacPacketTypeSeqHeader
	}

	return info, nil
}

// decodeMetaData декодирует payload NetStreamSetDataFrame (AMF0: имя + объект/ECMA-массив).
func decodeMetaData(payload []byte) (name string, meta interface{}, err error) {
	dec := rtmpmsg.NewAMFDecoder(bytes.NewReader(payload), rtmpmsg.EncodingTypeAMF0)

	if err := dec.Decode(&name); err != nil {
		return "", nil, fmt.Errorf("failed to decode onMetaData name: %w", err)
	}

	if err := dec.Decode(&meta); err != nil {
		return name, nil, fmt.Errorf("failed to decode onMetaData body: %w", err)
	}

	return name, meta, nil
}
