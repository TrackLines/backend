package es.tracklin.Client;

import org.springframework.stereotype.Component;

@Component
public class Client {
    private final long clientId;
    private final String token;

    public Client() {
        this.clientId = 0;
        this.token = "";
    }

    public int login(LoginModel loginModel) {
        return 0;
    }

    public long getClientId() {
        return clientId;
    }

    public String getToken() {
        return token;
    }
}
