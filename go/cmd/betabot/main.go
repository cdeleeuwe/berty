package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	qrterminal "github.com/mdp/qrterminal/v3"
	"google.golang.org/grpc"
	"moul.io/srand"

	"berty.tech/berty/v2/go/pkg/bertymessenger"
)

var (
	nodeAddr     = flag.String("addr", "127.0.0.1:9091", "remote 'berty daemon' address")
	displayName  = flag.String("display-name", safeDefaultDisplayName(), "bot's display name")
	conversation = flag.String("conversation", "", "conversation to join")
)

type ConvWithCount struct {
	ConversationPublicKey string
	ContactPublicKey      string
	ContactDisplayName    string
	Count                 int
	IsOneToOne            bool
}

var (
	convs              = []ConvWithCount{}
	staffConvPk string = ""
)

func main() {
	err := betabot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}

func betabot() error {
	rand.Seed(srand.Secure())
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// open gRPC connection to the remote 'berty daemon' instance
	var messengerClient bertymessenger.MessengerServiceClient
	{
		cc, err := grpc.Dial(*nodeAddr, grpc.WithInsecure())
		if err != nil {
			return fmt.Errorf("cannot connect to remote berty daemon: %w", err)
		}
		messengerClient = bertymessenger.NewMessengerServiceClient(cc)
	}

	// get sharing link
	{
		req := &bertymessenger.InstanceShareableBertyID_Request{DisplayName: *displayName}
		res, err := messengerClient.InstanceShareableBertyID(ctx, req)
		if err != nil {
			return err
		}
		log.Printf("berty id: %s", res.HTMLURL)

		qrterminal.GenerateHalfBlock(res.HTMLURL, qrterminal.L, os.Stdout)
	}

	// join conv in flag (generally multimember staff conv)
	{
		req := &bertymessenger.ConversationJoin_Request{
			Link: *conversation,
		}
		_, err := messengerClient.ConversationJoin(ctx, req)
		if err != nil {
			return err
		}

		// Recup staffConvPk
		link := req.GetLink()
		if link == "" {
			return fmt.Errorf("cannot get link")
		}

		query, method, err := bertymessenger.NormalizeDeepLinkURL(req.Link)
		if err != nil {
			return err
		}

		var pdlr *bertymessenger.ParseDeepLink_Reply
		switch method {
		case "/group":
			pdlr, err = bertymessenger.ParseGroupInviteURLQuery(query)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid link input")
		}
		bgroup := pdlr.GetBertyGroup()
		gpkb := bgroup.GetGroup().GetPublicKey()
		staffConvPk = bytesToString(gpkb)
	}

	// event loop
	var wg sync.WaitGroup
	{
		s, err := messengerClient.EventStream(ctx, &bertymessenger.EventStream_Request{})
		if err != nil {
			return fmt.Errorf("failed to listen to EventStream: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				gme, err := s.Recv()
				if err != nil {
					cancel()
					log.Printf("stream error: %v", err)
					return
				}

				wg.Add(1)
				go func() {
					defer wg.Done()
					handleEvent(ctx, messengerClient, gme)
				}()
			}
		}()
	}

	waitForCtrlC(ctx, cancel)
	wg.Wait()
	return nil
}

func handleEvent(ctx context.Context, messengerClient bertymessenger.MessengerServiceClient, gme *bertymessenger.EventStream_Reply) {
	switch gme.Event.Type {
	case bertymessenger.StreamEvent_TypeContactUpdated:
		// parse event's payload
		update, err := gme.Event.UnmarshalPayload()
		if err != nil {
			log.Printf("handle event: %v", err)
			return
		}
		// auto-accept contact requests
		contact := update.(*bertymessenger.StreamEvent_ContactUpdated).Contact
		log.Printf("<<< %s: contact=%q conversation=%q name=%q", gme.Event.Type, contact.PublicKey, contact.ConversationPublicKey, contact.DisplayName)
		if contact.State == bertymessenger.Contact_IncomingRequest {
			req := &bertymessenger.ContactAccept_Request{PublicKey: contact.PublicKey}
			_, err = messengerClient.ContactAccept(ctx, req)
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
		} else if contact.State == bertymessenger.Contact_Established {
			// When contact was established, send message and a group invitation
			time.Sleep(2 * time.Second)
			convs = append(convs, ConvWithCount{
				ConversationPublicKey: contact.ConversationPublicKey,
				ContactPublicKey:      contact.PublicKey,
				Count:                 0,
				ContactDisplayName:    contact.DisplayName,
				IsOneToOne:            true,
			})
			userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
				Body: "Hey! 🙌 Welcome to the Berty beta version! 🎊 \nI’m here to help you on the onboarding process.\nLet’s have some little test together.\nOK ? Just type ‘yes’, to let me know you copy that.",
			})
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
			_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
				Type:                  bertymessenger.AppMessage_TypeUserMessage,
				Payload:               userMessage,
				ConversationPublicKey: contact.ConversationPublicKey,
			})
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
		}
	case bertymessenger.StreamEvent_TypeInteractionUpdated:
		// parse event's payload
		update, err := gme.Event.UnmarshalPayload()
		if err != nil {
			log.Printf("handle event: %v", err)
			return
		}
		interaction := update.(*bertymessenger.StreamEvent_InteractionUpdated).Interaction
		log.Printf("<<< %s: conversation=%q", gme.Event.Type, interaction.ConversationPublicKey)
		switch {
		case interaction.Type == bertymessenger.AppMessage_TypeUserMessage && !interaction.IsMe && !interaction.Acknowledged:
			var conv *ConvWithCount
			for i := range convs {
				if convs[i].ConversationPublicKey == interaction.ConversationPublicKey {
					conv = &convs[i]
				}
			}
			interactionPayload, err := interaction.UnmarshalPayload()
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
			receivedMessage := interactionPayload.(*bertymessenger.AppMessage_UserMessage)
			log.Printf("userMessage [%s], conv [%v], convs [%v]", receivedMessage.GetBody(), conv, convs)
			if conv != nil && conv.IsOneToOne {
				switch {
				case conv.Count == 0 && checkValidationMessage(receivedMessage.GetBody()):
					conv.Count++
					time.Sleep(1 * time.Second)
					userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
						Body: "OK, perfect! 🤙 \nSo, would you like me to invite you in a group, to test multimember conversations? Type ‘yes’ to receive it!",
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
					_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
						Type:                  bertymessenger.AppMessage_TypeUserMessage,
						Payload:               userMessage,
						ConversationPublicKey: interaction.ConversationPublicKey,
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
				case conv.Count == 1 && checkValidationMessage(receivedMessage.GetBody()):
					conv.Count++
					time.Sleep(1 * time.Second)
					userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
						Body: "OK, I invite you! And I’ll also invite some staff members to join the group! I’m cool, but humans are sometimes more cool than me… :) <3",
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
					_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
						Type:                  bertymessenger.AppMessage_TypeUserMessage,
						Payload:               userMessage,
						ConversationPublicKey: interaction.ConversationPublicKey,
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}

					// TODO create with real staff group (in this group, betabot auto-reply will be disable)
					time.Sleep(1 * time.Second)
					{
						// create staff conversation
						createdConv, err := messengerClient.ConversationCreate(ctx, &bertymessenger.ConversationCreate_Request{
							DisplayName: fmt.Sprintf("staff-x-%s", conv.ContactDisplayName),
							ContactsToInvite: []string{
								conv.ContactPublicKey,
							},
						})
						if err != nil {
							log.Printf("handle event: %v", err)
							return
						}
						convs = append(convs, ConvWithCount{
							ConversationPublicKey: createdConv.PublicKey,
							IsOneToOne:            false,
						})
					}

					time.Sleep(1 * time.Second)
					userMessage, err = proto.Marshal(&bertymessenger.AppMessage_UserMessage{
						Body: "Also, would you like me to invite you in the Berty Community Group Chat ?\nJust type ‘yes’, if you want to join it! 😃",
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
					_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
						Type:                  bertymessenger.AppMessage_TypeUserMessage,
						Payload:               userMessage,
						ConversationPublicKey: interaction.ConversationPublicKey,
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
				case conv.Count == 2 && checkValidationMessage(receivedMessage.GetBody()):
					// TODO invitation to real berty-community (in this group, betabot auto-reply will be disable)
					conv.Count++
					time.Sleep(1 * time.Second)
					_, err = messengerClient.ConversationCreate(ctx, &bertymessenger.ConversationCreate_Request{
						DisplayName: "berty-community",
						ContactsToInvite: []string{
							conv.ContactPublicKey,
						},
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}

					time.Sleep(1 * time.Second)
					userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
						Body: "OK, it’s done! Welcome here, and congrats for joining our community! 👏👍🔥\nType /help when you need infos about available test commands! 📖",
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
					_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
						Type:                  bertymessenger.AppMessage_TypeUserMessage,
						Payload:               userMessage,
						ConversationPublicKey: interaction.ConversationPublicKey,
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
					log.Printf("Scenario finished !%v", convs)
				case conv.Count >= 3 && receivedMessage.GetBody() == "/help":
					userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
						Body: "In this conversation, you can type all theses commands :\n/demo group\n/demo demo\n/demo share\n/demo contact \"Here is the QR code of manfred, just add him!\"",
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}

					_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
						Type:                  bertymessenger.AppMessage_TypeUserMessage,
						Payload:               userMessage,
						ConversationPublicKey: interaction.ConversationPublicKey,
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
				default:
					answers := getAnswersRange()
					// auto-reply to user's messages
					if err != nil {
						log.Printf("Failed to generate randome number: %v", err)
						return
					}
					userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
						Body: answers[rand.Intn(len(answers))], // nolint:gosec // we need to use math/rand here, but it is seeded from crypto/rand
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}

					_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
						Type:                  bertymessenger.AppMessage_TypeUserMessage,
						Payload:               userMessage,
						ConversationPublicKey: interaction.ConversationPublicKey,
					})
					if err != nil {
						log.Printf("handle event: %v", err)
						return
					}
				}
			}
		case interaction.Type == bertymessenger.AppMessage_TypeGroupInvitation && !interaction.IsMe:
			// auto-accept invitations to group
			interactionPayload, err := interaction.UnmarshalPayload()
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
			receivedInvitation := interactionPayload.(*bertymessenger.AppMessage_GroupInvitation)
			_, err = messengerClient.ConversationJoin(ctx, &bertymessenger.ConversationJoin_Request{
				Link: receivedInvitation.GetLink(),
			})
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
			log.Printf("GroupInvit: %q", interaction)
			return
		}
	case bertymessenger.StreamEvent_TypeConversationUpdated:
		// send to multimember staff conv that this user join us on Berty with the link of the group
		// parse event's payload
		update, err := gme.Event.UnmarshalPayload()
		if err != nil {
			log.Printf("handle event: %v", err)
			return
		}
		conversation := update.(*bertymessenger.StreamEvent_ConversationUpdated).Conversation
		log.Printf("<<< %s: conversation=%q", gme.Event.Type, conversation)
		if strings.Contains(conversation.GetDisplayName(), "staff-x-") {
			userName := strings.Split(conversation.GetDisplayName(), "-")[2]
			userMessage, err := proto.Marshal(&bertymessenger.AppMessage_UserMessage{
				Body: fmt.Sprintf("Hi guys, we have a new user in our community! 🥳\nSay hello to %s! 👍\nFor the moment i can't send a group invitation so i share the link of the conversation here: %s", userName, conversation.GetLink()),
			})
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}
			_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
				Type:                  bertymessenger.AppMessage_TypeUserMessage,
				Payload:               userMessage,
				ConversationPublicKey: staffConvPk,
			})
			if err != nil {
				log.Printf("handle event: %v", err)
				return
			}

			// TODO send the group invitation, maybe the group invitation was broken so for the moment i sent the link in a message

			// time.Sleep(2 * time.Second)
			// {
			// 	log.Printf("Link: %s", conversation.GetLink())
			// 	groupInvitation, err := proto.Marshal(&bertymessenger.AppMessage_GroupInvitation{
			// 		Link: conversation.GetLink(),
			// 	})
			// 	if err != nil {
			// 		log.Printf("handle event: %v", err)
			// 		return
			// 	}
			// 	_, err = messengerClient.Interact(ctx, &bertymessenger.Interact_Request{
			// 		Type:                  bertymessenger.AppMessage_TypeGroupInvitation,
			// 		Payload:               groupInvitation,
			// 		ConversationPublicKey: staffConvPk,
			// 	})
			// 	if err != nil {
			// 		log.Printf("handle event: %v", err)
			// 		return
			// 	}
			// }
		}
	default:
		log.Printf("<<< %s: ignored", gme.Event.Type)
	}
}

func checkValidationMessage(s string) bool {
	switch strings.ToLower(s) {
	case "y", "yes", "yes!":
		return true
	default:
		return false
	}
}

func waitForCtrlC(ctx context.Context, cancel context.CancelFunc) {
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, os.Interrupt)

	select {
	case <-signalChannel:
		cancel()
	case <-ctx.Done():
	}
}

func bytesToString(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func safeDefaultDisplayName() string {
	var name string
	current, err := user.Current()
	if err == nil {
		name = current.Username
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		name = "Anonymous4242"
	}
	return fmt.Sprintf("%s (bot)", name)
}

func getAnswersRange() []string {
	return []string{
		"Welcome to the beta!",
		"Hello! Welcome to Berty!",
		"Hey, I hope you're feeling well here!",
		"Hi, I'm here for you at anytime for tests!",
		"Hello dude!",
		"Hello :)",
		"Ow, I like to receive test messages <3",
		"What's up ?",
		"How r u ?",
		"Hello, 1-2, 1-2, check, check?!",
		"Do you copy ?",
		"If you say ping, I'll say pong.",
		"I'm faster than you at sending message :)",
		"One day, bots will rules the world. Or not.",
		"You're so cute.",
		"I like discuss with you, I feel more and more clever.",
		"I'm so happy to chat with you.",
		"I could chat with you all day long.",
		"Yes darling ? Can I help you ?",
		"OK, copy that.",
		"OK, I understand.",
		"Hmmm, Hmmmm. One more time ?",
		"I think you're the most clever human I know.",
		"I missed you babe.",
		"OK, don't send me nudes, I'm a bot dude.",
		"Come on, let's party.",
		"May we have a chat about our love relationship future ?",
		"That's cool. I copy.",
	} // 28
}