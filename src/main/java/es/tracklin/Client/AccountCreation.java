package es.tracklin.Client;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.security.MessageDigest;
import java.util.Date;

@Component
public class AccountCreation {
    @Autowired
    es.tracklin.Configuration.Tokens tokens;

    private String username;
    private String password;
    private String email;
    private String number;

    public void setTokens(es.tracklin.Configuration.Tokens tokens) {
        this.tokens = tokens;
    }

    public String getUsername() {
        return username;
    }

    public void setUsername(String username) {
        this.username = username;
    }

    public String getPassword() {
        return password;
    }

    public void setPassword(String password) {
        this.password = password;
    }

    public String getEmail() {
        return email;
    }

    public void setEmail(String email) {
        this.email = email;
    }

    public String getNumber() {
        return number;
    }

    public void setNumber(String number) {
        this.number = number;
    }

    public String web(String username) {
        return tokens.generateToken(username, "web");
    }

    public String api(String username) {
        return tokens.generateToken(username, "api");
    }

    public int createAccount() {
        return 0;
    }
}
