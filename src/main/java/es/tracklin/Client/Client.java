package es.tracklin.Client;

import es.tracklin.Configuration.MongoConfiguration;
import org.springframework.context.annotation.Import;
import org.springframework.stereotype.Component;

@Component
@Import(MongoConfiguration.class)
public class Client {
    private final MongoConfiguration mongoConfiguration;
    private final long clientId;
    private final String token;

    public Client(MongoConfiguration mongoConfiguration) {
        this.mongoConfiguration = mongoConfiguration;
        this.clientId = 0;
        this.token = "";
    }

    public int login(LoginModel loginModel) {
        return mongoConfiguration.getPort();
    }

    public long getClientId() {
        return clientId;
    }

    public String getToken() {
        return token;
    }
}
