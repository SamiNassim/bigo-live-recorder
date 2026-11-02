package bigo

import "errors"

// opusSamples reads packet duration at Opus's 48 kHz clock. Compressed frames
// remain untouched; this only supplies timestamps for container packaging.
func opusSamples(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, errors.New("empty Opus packet")
	}
	config := data[0] >> 3
	var samples int
	switch {
	case config >= 16:
		samples = 120 << (config & 3)
	case config >= 12:
		samples = 480 << (config & 1)
	default:
		samples = [4]int{480, 960, 1920, 2880}[config&3]
	}
	count := 1
	switch data[0] & 3 {
	case 1, 2:
		count = 2
	case 3:
		if len(data) < 2 {
			return 0, errors.New("truncated Opus packet duration")
		}
		count = int(data[1] & 63)
	}
	if count == 0 || count*samples > 5760 {
		return 0, errors.New("invalid Opus packet duration")
	}
	return count * samples, nil
}

func decodeGuestAudio(packet nativeAudioPacket) ([]nativeMediaFrame, error) {
	if packet.codec == 6 {
		return decodeNativeAAC(packet)
	}
	if packet.codec != 7 {
		return nil, errors.New("unsupported native guest audio codec")
	}
	var frames []nativeMediaFrame
	elapsed := 0
	for _, sample := range packet.samples[:packet.count] {
		if len(sample.data) == 0 {
			continue
		}
		samples, err := opusSamples(sample.data)
		if err != nil {
			return nil, err
		}
		pts := packet.pts + uint32(elapsed/48)
		frames = append(frames, nativeMediaFrame{data: sample.data, pts: pts, dts: pts, codec: 7})
		elapsed += samples
	}
	return frames, nil
}

// MPEG-TS Opus access-unit framing precedes the original compressed packet.
func opusAccessUnit(data []byte) []byte {
	result := make([]byte, 0, len(data)+3+len(data)/255)
	result = append(result, 0x7f, 0xe0)
	for remaining := len(data); ; remaining -= 255 {
		if remaining < 255 {
			result = append(result, byte(remaining))
			break
		}
		result = append(result, 255)
	}
	return append(result, data...)
}
