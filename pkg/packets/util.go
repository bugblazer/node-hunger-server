package packets

import "server/internal/server/objects"

type Msg = isPacket_Msg

func NewChat(msg string) Msg {
	return &Packet_Chat{
		Chat: &ChatMessage{
			Msg: msg,
		},
	}
}

func NewId(id uint64) Msg {
	return &Packet_Id{
		Id: &IdMessage{
			Id: id,
		},
	}
}

func NewOkResponse() Msg {
	return &Packet_OkResponse{
		OkResponse: &OkResponseMessage{},
	}
}

func NewDenyResponse(reason string) Msg {
	return &Packet_DenyResponse{
		DenyResponse: &DenyResponseMessage{
			Reason: reason,
		},
	}
}

func NewPlayer(id uint64, player *objects.Player) Msg {
	return &Packet_Player{
		Player: &PlayerMessage{
			Id:        id,
			Name:      player.Name,
			X:         player.X,
			Y:         player.Y,
			Radius:    player.Radius,
			Direction: player.Direction,
			Speed:     player.Speed,
			Color:     player.Color,
		},
	}
}

func newSporeMessage(spore_id uint64, spore *objects.Spore) *SporeMessage {
	return &SporeMessage{
		Id:          spore_id,
		X:           spore.X,
		Y:           spore.Y,
		Radius:      spore.Radius,
		Ejected:     spore.Ejected,
		FromX:       spore.FromX,
		FromY:       spore.FromY,
		OwnerId:     spore.OwnerId,
		LockSeconds: spore.LockFor.Seconds(),
	}
}

func NewSpore(id uint64, spore *objects.Spore) Msg {
	return &Packet_Spore{
		newSporeMessage(id, spore),
	}
}

func NewSporesBatch(spores map[uint64]*objects.Spore) Msg {
	sporesMessages := make([]*SporeMessage, 0, len(spores))
	for id, spore := range spores {
		sporesMessages = append(sporesMessages, newSporeMessage(id, spore))
	}

	return &Packet_SporesBatch{
		SporesBatch: &SporesBatchMessage{
			Spores: sporesMessages,
		},
	}
}

func NewHiscoreBoard(hiscores []*HiscoreMessage) Msg {
	return &Packet_HiscoreBoard{
		HiscoreBoard: &HiscoreBoardMessage{
			Hiscores: hiscores,
		},
	}
}

func NewDisconnect(reason string) Msg {
	return &Packet_Disconnect{
		Disconnect: &DisconnectMessage{
			Reason: reason,
		},
	}
}

func newVirusMessage(id uint64, virus *objects.Virus) *VirusMessage {
	return &VirusMessage{
		Id:     id,
		X:      virus.X,
		Y:      virus.Y,
		Radius: virus.Radius,
	}
}

func NewVirus(id uint64, virus *objects.Virus) Msg {
	return &Packet_Virus{
		Virus: newVirusMessage(id, virus),
	}
}

func NewVirusesBatch(viruses map[uint64]*objects.Virus) Msg {
	virusMessages := make([]*VirusMessage, 0, len(viruses))
	for id, virus := range viruses {
		virusMessages = append(virusMessages, newVirusMessage(id, virus))
	}

	return &Packet_VirusesBatch{
		VirusesBatch: &VirusesBatchMessage{
			Viruses: virusMessages,
		},
	}
}

func NewVirusConsumed(virusId, playerId uint64) Msg {
	return &Packet_VirusConsumed{
		VirusConsumed: &VirusConsumedMessage{
			VirusId:  virusId,
			PlayerId: playerId,
		},
	}
}
