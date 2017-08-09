package es.tracklin.DataLayer.Database;

import es.tracklin.Backend;
import es.tracklin.Client.ClientData;
import es.tracklin.Client.AccountCreation;
import es.tracklin.DataConnections.Database;

import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.junit4.SpringRunner;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class DataTests {
    @Autowired
    es.tracklin.Configuration.Database databaseConfiguration;

    @Autowired
    es.tracklin.Configuration.Tokens tokensConfiguration;

    private String username = "bob";
    private String password = "bob";
    private String email = "bob@bob.bob";

    @Test
    public void shouldAddUser() throws Exception {
        Database database = new Database();
        database.setDatabaseConfiguration(databaseConfiguration);

        ClientData clientData = new ClientData();
        ClientData.Credentials credentials = clientData.new Credentials();
        ClientData.ContactDetails contactDetails = clientData.new ContactDetails();

        credentials.setPassword(password);
        credentials.setUsername(username);
        clientData.setCredentials(credentials);

        AccountCreation creation = new AccountCreation();
//        creation.setTokens(tokensConfiguration);
//        String web = creation.web(username);
//        String api = creation.api(username);
    }

    @Test
    public void shouldUpdateUser() throws Exception {

    }

    @Test
    public void shouldDeleteUser() throws Exception {

    }
}
