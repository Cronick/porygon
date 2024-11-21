package main

import (
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"porygon/api"
	"porygon/config"
	"porygon/database"
	"porygon/discord"
)

func saveMessageIDs(filename string, data map[string]string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	err = encoder.Encode(data)
	if err != nil {
		return err
	}

	return nil
}

func loadMessageIDs(filename string) map[string]string {
	messageIDs := make(map[string]string)

	if _, err := os.Stat(filename); os.IsNotExist(err) {
		file, err := os.Create(filename)
		if err != nil {
			log.Println("error creating message IDs file:", err)
			return messageIDs
		}
		defer file.Close()

		json.NewEncoder(file).Encode(messageIDs)
	} else {
		file, err := os.Open(filename)
		if err != nil {
			log.Println("error opening message IDs file:", err)
			return messageIDs
		}
		defer file.Close()

		json.NewDecoder(file).Decode(&messageIDs)
	}

	return messageIDs
}

func gatherStats(db *sqlx.DB, config config.Config) (discord.GatheredStats, error) {
	start := time.Now()
	var err error
	var gathered discord.GatheredStats

	gathered.Pokemon, err = database.GetPokeStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.RaidEgg, err = database.GetRaidStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Gym, err = database.GetGymStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Pokestop, err = database.GetPokestopStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Reward, err = database.GetRewardStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Lure, err = database.GetLureStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Rocket, err = database.GetRocketStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Event, err = database.GetEventStats(db)
	if err != nil {
		return gathered, err
	}

	gathered.Route, err = database.GetRoutesStats(db)
	if err != nil {
		return gathered, err
	}

	// probs break this out into query? again idk how to handle passing the config well just yet
	if config.Config.IncludeActiveCounts {
		hundoApiResponses, err := api.ApiRequest(config, 15, 15)
		if err != nil {
			return gathered, err
		}

		hundoSpawnIds := make(map[int]bool)
		for _, apiResponse := range hundoApiResponses {
			hundoSpawnIds[apiResponse.SpawnId] = true
		}
		gathered.HundoActiveCount = len(hundoSpawnIds)

		nundoApiResponses, err := api.ApiRequest(config, 0, 0)
		if err != nil {
			return gathered, err
		}

		nundoSpawnIds := make(map[int]bool)
		for _, apiResponse := range nundoApiResponses {
			nundoSpawnIds[apiResponse.SpawnId] = true
		}
		gathered.NundoActiveCount = len(nundoSpawnIds)
	}

	elapsed := time.Since(start)
	log.Printf("Fetched stats in %s\n", elapsed)
	return gathered, nil
}

func main() {
	var c config.Config
	log.Println("Starting porygon")
	if err := c.ParseConfig(); err != nil {
		log.Panicln(err)
	}

	messageIDs := loadMessageIDs("messageIDs.json")

	dg, err := discordgo.New("Bot " + c.Discord.Token)

	if err != nil {
		log.Println("error creating Discord session,", err)
		return
	}

	defer dg.Close()

	log.Println("Add slash commands handlers")
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if h, ok := discord.CommandHandlers[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})

	log.Println("Open Discord connection")
	err = dg.Open()
	if err != nil {
		log.Println("error opening connection,", err)
		return
	}

	log.Println("Register commands")
	registeredCommands := make([]*discordgo.ApplicationCommand, len(discord.Commands))
	for i, v := range discord.Commands {
		cmd, err := dg.ApplicationCommandCreate(dg.State.User.ID, "", v)
		if err != nil {
			log.Printf("Cannot create '%v' command: %v\n", v.Name, err)
		}
		registeredCommands[i] = cmd
	}

	log.Println("Start loop")
	go func() {
		for {
			db, err := database.DbConn(c)
			if err != nil {
				log.Println("error connecting to MariaDB,", err)
				time.Sleep(time.Duration(c.Config.ErrorRefreshInterval) * time.Second)
				continue
			}
			gathered, err := gatherStats(db, c)
			db.Close()

			if err != nil {
				log.Println("failed to fetch stats,", err)
				time.Sleep(time.Duration(c.Config.ErrorRefreshInterval) * time.Second)
				continue
			}

			fields := discord.GenerateFields(gathered, c)

			embed := &discordgo.MessageEmbed{
				Title:     c.Config.EmbedTitle,
				Fields:    fields,
				Timestamp: time.Now().Format(time.RFC3339),
			}

			for _, channelID := range c.Discord.ChannelIDs {
				var msg *discordgo.Message
				var err error
				var msgID string
				var ok bool

				// Check if we already have a message ID for this channel
				if msgID, ok = messageIDs[channelID]; ok {

					// Try to edit the existing message
					msg, err = dg.ChannelMessageEditEmbed(channelID, msgID, embed)

					if err != nil {
						// Check if the error is related to an invalid message ID
						if strings.Contains(err.Error(), "Unknown Message") {

							// Check if DeleteOldEmbeds is enabled, and then clean up channel before sending new message
							if c.Config.DeleteOldEmbeds {
								messages, err := dg.ChannelMessages(channelID, 100, "", "", "")
								if err == nil {
									for _, msg := range messages {
										_ = dg.ChannelMessageDelete(channelID, msg.ID)
									}
								}
							}

							// Message ID is no longer valid, send a new message
							msg, err = dg.ChannelMessageSendEmbed(channelID, embed)

							if err != nil {
								log.Println("Error sending new embed in channel", channelID, ":", err)
								continue
							}
						} else {
							// Other error cases
							log.Println("Error editing embed in channel", channelID, ":", err)
							continue
						}
					}
				} else {

					// Check if DeleteOldEmbeds is enabled, and then clean up channel before sending new message
					if c.Config.DeleteOldEmbeds {
						messages, err := dg.ChannelMessages(channelID, 100, "", "", "")
						if err == nil {
							for _, msg := range messages {
								_ = dg.ChannelMessageDelete(channelID, msg.ID)
							}
						}
					}

					// No existing message ID, send a new message
					msg, err = dg.ChannelMessageSendEmbed(channelID, embed)
					if err != nil {
						log.Println("Error sending embed in channel", channelID, ":", err)
						continue
					}
				}

				// Update the message ID map if necessary
				if msgID == "" || msgID != msg.ID {
					messageIDs[channelID] = msg.ID
					if err := saveMessageIDs("messageIDs.json", messageIDs); err != nil {
						log.Println("Error saving message IDs:", err)
					}
				}
			}

			time.Sleep(time.Duration(c.Config.RefreshInterval) * time.Second)
		}
	}()

	log.Println("Porygon is now running. Press CTRL-C to exit.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Received signal. Exiting...")
}
