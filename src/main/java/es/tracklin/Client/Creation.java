package es.tracklin.Client;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.security.MessageDigest;
import java.util.Date;

@Component
public class Creation {
    @Autowired
    es.tracklin.Configuration.Tokens tokens;

    public void setTokens(es.tracklin.Configuration.Tokens tokens) {
        this.tokens = tokens;
    }

    public String web(String username) {
        return tokens.generateToken(username, "web");
    }

    public String api(String username) {
        return tokens.generateToken(username, "api");
    }


}
