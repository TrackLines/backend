package es.tracklin.Mongo;

import es.tracklin.Backend;
import es.tracklin.Client.ClientData;
import es.tracklin.Configuration.MongoConfiguration;
import es.tracklin.DataConnections.Mongo;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.junit4.SpringRunner;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class DataTests {
    @Autowired
    MongoConfiguration mongoConfiguration;

    @Test
    public void shouldInsertData() throws Exception {
        Mongo mongo = new Mongo();
        ClientData clientData = new ClientData();
        ClientData.Tokens tokens = clientData.new Tokens();
        ClientData.ContactDetails contactDetails = clientData.new ContactDetails();

        clientData.setName("bob");
        clientData.setPassword("bob");

        contactDetails.setEmail("bob@bob.bob");
        contactDetails.setName("bobster");
        contactDetails.setNumber("00");
        clientData.setContactDetails(contactDetails);

        tokens.setAPI("bob");
        tokens.setInterface("bob");
        clientData.setTokens(tokens);

        String returnData = mongo.addUser(clientData);
    }
}
