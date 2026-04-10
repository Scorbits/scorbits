package email

import (
	"crypto/tls"
	"fmt"
	"net/smtp"
	"os"
	"strings"
)

func Send(to, subject, body string) error {
	from := os.Getenv("SMTP_EMAIL")
	password := os.Getenv("SMTP_PASSWORD")
	fromLabel := os.Getenv("SMTP_FROM")
	if fromLabel == "" {
		fromLabel = "Scorbits <" + from + ">"
	}
	// Supprimer les espaces du mot de passe d'application Google
	password = strings.ReplaceAll(password, " ", "")

	host := "smtp.gmail.com"
	port := "465"
	addr := host + ":" + port

	msg := "From: " + fromLabel + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" + body

	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         host,
	}

	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("erreur connexion SMTP: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("erreur client SMTP: %w", err)
	}
	defer client.Quit()

	auth := smtp.PlainAuth("", from, password, host)
	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("erreur auth SMTP: %w", err)
	}
	if err = client.Mail(from); err != nil {
		return fmt.Errorf("erreur MAIL FROM: %w", err)
	}
	if err = client.Rcpt(to); err != nil {
		return fmt.Errorf("erreur RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("erreur DATA: %w", err)
	}
	_, err = w.Write([]byte(msg))
	if err != nil {
		return fmt.Errorf("erreur écriture: %w", err)
	}
	return w.Close()
}

func SendVerification(to, pseudo, token string) error {
	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	link := baseURL + "/verify?token=" + token
	subject := "Scorbits — Confirmez votre adresse email"
	body := `<!DOCTYPE html>
<html>
<head><meta charset="UTF-8"></head>
<body style="background:#030a04;color:#cffadb;font-family:Inter,sans-serif;margin:0;padding:0;">
<div style="text-align:center;padding:20px 0 10px;">
  <img src="https://scorbits.com/static/scorbits_logo.png" alt="Scorbits" style="height:60px;width:auto;">
</div>
<div style="max-width:520px;margin:0 auto 40px;background:#071009;border:1px solid #006628;border-radius:12px;overflow:hidden;">
  <div style="background:#003314;padding:2rem;text-align:center;border-bottom:1px solid #006628;">
    <div style="font-size:1.4rem;font-weight:700;color:#cffadb;">Scorbits</div>
  </div>
  <div style="padding:2rem;">
    <p style="font-size:1.1rem;font-weight:600;color:#cffadb;margin-bottom:0.5rem;">Bonjour @` + pseudo + ` 👋</p>
    <p style="color:#7ab98a;line-height:1.7;margin-bottom:1.5rem;">
      Bienvenue sur Scorbits ! Pour activer votre compte et accéder à votre wallet SCO, confirmez votre adresse email en cliquant sur le bouton ci-dessous.
    </p>
    <div style="text-align:center;margin:2rem 0;">
      <a href="` + link + `" style="background:#00e85a;color:#000;text-decoration:none;padding:14px 32px;border-radius:7px;font-weight:700;font-size:1rem;display:inline-block;">
        Confirmer mon adresse email
      </a>
    </div>
    <p style="color:#2d6b3f;font-size:0.82rem;line-height:1.6;">
      Si le bouton ne fonctionne pas, copiez ce lien dans votre navigateur :<br>
      <span style="color:#00b847;word-break:break-all;">` + link + `</span>
    </p>
    <p style="color:#2d6b3f;font-size:0.78rem;margin-top:1.5rem;">
      Ce lien est valable 24 heures. Si vous n'avez pas créé de compte Scorbits, ignorez cet email.
    </p>
  </div>
  <div style="background:#030a04;padding:1rem;text-align:center;border-top:1px solid #0d2e15;">
    <p style="color:#2d6b3f;font-size:0.75rem;margin:0;">Scorbits (SCO) — Proof of Work — Résistance du temps</p>
  </div>
</div>
</body>
</html>`
	return Send(to, subject, body)
}